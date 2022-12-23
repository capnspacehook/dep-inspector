package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"

	"golang.org/x/exp/slices"
	"golang.org/x/tools/go/packages"
)

var (
	unwantedPkgs = []string{
		"C",
		"embed",
		"os/signal",
		"os/exec",
		"runtime",
		"runtime/debug",
		"runtime/metrics",
		"runtime/pprof",
		"runtime/trace",
		"reflect",
		"unsafe",
	}
	unwantedFuncs = map[string][]string{
		"runtime": {
			"Breakpoint",
			"GC",
			"GOMAXPROCS",
			"LockOSThread",
			"MemProfile",
			"MutexProfile",
			"ReadMemStats",
			"ReadTrace",
			"SetBlockProfileRate",
			"SetCPUProfileRate",
			"SetCgoTraceback",
			"SetFinalizer",
			"SetMutexProfileFraction",
			"StartTrace",
			"StopTrace",
			"ThreadCreateProfile",
			"UnlockOSThread",
		},
	}
)

type packagesInfo map[string]*listedPackage

type listedPackage struct {
	Dir        string
	ImportPath string
	Name       string
	Module     listedModule
	Standard   bool
	Imports    []string
	Deps       []string
	Incomplete bool
}

type listedModule struct {
	Path    string
	Version string
}

func listPackages(pkgs ...string) (packagesInfo, error) {
	var listBuf bytes.Buffer
	cmd := []string{"go", "list", "-deps", "-json"}
	cmd = append(cmd, pkgs...)
	err := runCommand(&listBuf, false, cmd...)
	if err != nil {
		return nil, fmt.Errorf("error listing dependencies: %v", err)
	}

	dec := json.NewDecoder(&listBuf)
	listedPkgs := make(packagesInfo)
	for dec.More() {
		var pkg listedPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("error decoding: %v", err)
		}
		listedPkgs[pkg.ImportPath] = &pkg
	}

	return listedPkgs, nil
}

type packageIssue struct {
	srcPkg      string
	pkgChain    []string
	unwantedPkg string
	calls       []funcCall
}

type funcCall struct {
	position token.Position
	source   string
}

func findUnwantedImports(dep string, pkgs packagesInfo) ([]packageIssue, error) {
	var depPkgs []string
	for _, pkg := range pkgs {
		if !pkg.Standard && pkg.Module.Path == dep {
			depPkgs = append(depPkgs, pkg.ImportPath)
		}
	}
	slices.Sort(depPkgs)

	// TODO: use this instead of 'go list'?
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
			packages.NeedDeps | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo,
	}
	// TODO: make map of import path -> *packages.Package
	// TODO: how does 'go list' know if a package is from stdlib?
	// by the path to it's source code, if in GOROOT its stdlib
	loadedPkgs, err := packages.Load(cfg, depPkgs...)
	if err != nil {
		return nil, fmt.Errorf("error loading packages: %v", err)
	}

	seen := make(map[string]struct{})
	pkgIssues, err := searchPkgs(depPkgs, pkgs, loadedPkgs, nil, seen)
	if err != nil {
		return nil, fmt.Errorf("error searching for unwanted imports: %v", err)
	}

	return pkgIssues, nil
}

func searchPkgs(pkgs []string, pkgInfo packagesInfo, loadedPkgs []*packages.Package, stack []string, seen map[string]struct{}) ([]packageIssue, error) {
	var pkgIssues []packageIssue
	for _, pkgPath := range pkgs {
		if _, ok := seen[pkgPath]; ok {
			continue
		}
		pkg, ok := pkgInfo[pkgPath]
		if !ok {
			return nil, fmt.Errorf("could not find package %s", pkgPath)
		}
		if pkg.Standard {
			continue
		}
		seen[pkg.ImportPath] = struct{}{}

		foundImps := findIn(pkg.Imports, unwantedPkgs)
		for _, f := range foundImps {
			pkgIssue := packageIssue{
				srcPkg:      pkgPath,
				unwantedPkg: f,
			}
			if len(stack) > 0 {
				pkgIssue.pkgChain = append(stack, pkgPath)
			}

			idx := slices.IndexFunc(loadedPkgs, func(p *packages.Package) bool {
				return p.PkgPath == pkgPath
			})
			if idx == -1 {
				return nil, fmt.Errorf("could not find package %s", pkgPath)
			}
			calls, err := findFuncCalls(loadedPkgs[idx])
			if err != nil {
				return nil, fmt.Errorf("error finding function calls: %v", err)
			}
			pkgIssue.calls = calls

			pkgIssues = append(pkgIssues, pkgIssue)
		}

		foundDeps := findIn(pkg.Deps, unwantedPkgs)
		if len(foundDeps) == 0 {
			continue
		}
		depIssues, err := searchPkgs(pkg.Deps, pkgInfo, loadedPkgs, append(stack, pkgPath), seen)
		if err != nil {
			return nil, err
		}
		pkgIssues = append(pkgIssues, depIssues...)
	}

	return pkgIssues, nil
}

func findIn(haystack, needles []string) []string {
	var found []string
	for _, needle := range needles {
		if slices.Contains(haystack, needle) {
			found = append(found, needle)
		}
	}

	return found
}

func findFuncCalls(pkg *packages.Package) ([]funcCall, error) {
	var calls []funcCall
	for _, file := range pkg.Syntax {
		var found bool
		for _, imp := range file.Imports {
			impPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("error unquoting: %v", err)
			}
			if slices.Contains(unwantedPkgs, impPath) {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			callExpr, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selExpr, ok := callExpr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selExpr.X.(*ast.Ident)
			if !ok {
				return true
			}
			pkgName, ok := pkg.TypesInfo.Uses[ident].(*types.PkgName)
			if !ok {
				return true
			}

			// log.Println(pkgName.Imported().Path())
			if !slices.Contains(unwantedPkgs, pkgName.Imported().Path()) {
				return true
			}

			calls = append(calls, funcCall{
				position: pkg.Fset.Position(callExpr.Pos()),
			})

			return true
		})
	}

	return calls, nil
}

func pkgIssuesEqual(a, b packageIssue) bool {
	if a.srcPkg != b.srcPkg {
		return false
	}
	if !slices.Equal(a.pkgChain, b.pkgChain) {
		return false
	}
	return a.unwantedPkg == b.unwantedPkg
}
