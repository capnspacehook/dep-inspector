package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strconv"

	"golang.org/x/exp/maps"
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
			"KeepAlive",
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
	position    token.Position
	sourceLines []string
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
		Mode: packages.NeedName | packages.NeedCompiledGoFiles | packages.NeedImports |
			packages.NeedDeps | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo,
	}
	// TODO: make map of import path -> *packages.Package
	// TODO: how does 'go list' know if a package is from stdlib?
	// by the path to it's source code, if in GOROOT it's stdlib
	loadedPkgs, err := packages.Load(cfg, depPkgs...)
	if err != nil {
		return nil, fmt.Errorf("error loading packages: %v", err)
	}

	pkgMap := make(map[string]*packages.Package)
	mapLoadedPkgs(loadedPkgs, pkgMap)

	seen := make(map[string]struct{})
	pkgIssues, err := searchPkgs(depPkgs, pkgs, pkgMap, nil, seen)
	if err != nil {
		return nil, fmt.Errorf("error searching for unwanted imports: %v", err)
	}

	return pkgIssues, nil
}

type loadedPackages map[string]*packages.Package

func mapLoadedPkgs(loadedPkgs []*packages.Package, pkgMap loadedPackages) {
	for _, loadedPkg := range loadedPkgs {
		if _, ok := pkgMap[loadedPkg.PkgPath]; ok {
			continue
		}

		pkgMap[loadedPkg.PkgPath] = loadedPkg
		mapLoadedPkgs(maps.Values(loadedPkg.Imports), pkgMap)
	}
}

func searchPkgs(pkgs []string, pkgInfo packagesInfo, loadedPkgs loadedPackages, stack []string, seen map[string]struct{}) ([]packageIssue, error) {
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
		for _, foundImp := range foundImps {
			pkg, ok := loadedPkgs[pkgPath]
			if !ok {
				return nil, fmt.Errorf("could not find package %s", pkgPath)
			}
			calls, err := findFuncCalls(foundImp, pkg)
			if err != nil {
				return nil, fmt.Errorf("error finding function calls: %v", err)
			}
			if len(calls) == 0 {
				continue
			}

			pkgIssue := packageIssue{
				srcPkg:      pkgPath,
				unwantedPkg: foundImp,
			}
			if len(stack) > 0 {
				pkgIssue.pkgChain = append(stack, pkgPath)
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

func findFuncCalls(fromImp string, pkg *packages.Package) ([]funcCall, error) {
	var callsInPkg []funcCall
	for i, file := range pkg.Syntax {
		var found bool
		for _, imp := range file.Imports {
			impPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("error unquoting: %v", err)
			}
			if impPath == fromImp {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		var calls []funcCall
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

			if pkgName.Imported().Path() != fromImp {
				return true
			}
			specificFuncs, ok := unwantedFuncs[fromImp]
			if ok && !slices.Contains(specificFuncs, selExpr.Sel.Name) {
				return true
			}
			// if the last recorded function call is on the same line,
			// skip this call as the previous call's source lines will
			// cover it
			pos := pkg.Fset.PositionFor(callExpr.Pos(), false)
			if len(calls) > 0 && calls[len(calls)-1].position.Line == pos.Line {
				return true
			}

			calls = append(calls, funcCall{
				position: pos,
			})
			return true
		})

		// TODO: handle case when len(pkg.Syntax) != len(pkg.CompiledGoFiles)
		c, err := getCallsSrcLines(pkg.CompiledGoFiles[i], calls)
		if err != nil {
			return nil, err
		}
		callsInPkg = append(callsInPkg, c...)
	}

	return callsInPkg, nil
}

func getCallsSrcLines(path string, calls []funcCall) ([]funcCall, error) {
	srcFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error opening source file: %v", err)
	}
	defer srcFile.Close()
	l := newLineReader(srcFile)

	for j, call := range calls {
		src, err := getSrcLines(l, call.position.Line, call.position.Line)
		if err != nil {
			return nil, fmt.Errorf("error reading source file: %v", err)
		}

		calls[j].sourceLines = src
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
