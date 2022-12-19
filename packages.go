package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"

	"golang.org/x/exp/slices"
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
}

func findUnwantedImports(dep string, pkgs packagesInfo) ([]packageIssue, error) {
	var depPkgs []string
	for _, pkg := range pkgs {
		if !pkg.Standard && pkg.Module.Path == dep {
			depPkgs = append(depPkgs, pkg.ImportPath)
		}
	}
	seen := make(map[string]struct{})
	return searchPkgs(depPkgs, pkgs, nil, seen)
}

func searchPkgs(pkgs []string, pkgInfo packagesInfo, stack []string, seen map[string]struct{}) ([]packageIssue, error) {
	var pkgIssues []packageIssue
	for _, p := range pkgs {
		if _, ok := seen[p]; ok {
			continue
		}
		pkg, ok := pkgInfo[p]
		if !ok {
			return nil, fmt.Errorf("could not find package %s", p)
		}
		if pkg.Standard {
			continue
		}
		seen[pkg.ImportPath] = struct{}{}
		log.Println(p)

		foundImps := findIn(pkg.Imports, unwantedPkgs)
		for _, f := range foundImps {
			pkgIssue := packageIssue{
				srcPkg:      p,
				unwantedPkg: f,
			}
			if len(stack) > 0 {
				pkgIssue.pkgChain = append(stack, p)
			}
			pkgIssues = append(pkgIssues, pkgIssue)
		}

		foundDeps := findIn(pkg.Deps, unwantedPkgs)
		if len(foundDeps) == 0 {
			continue
		}
		depIssues, err := searchPkgs(pkg.Deps, pkgInfo, append(stack, p), seen)
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

func pkgIssuesEqual(a, b packageIssue) bool {
	if a.srcPkg != b.srcPkg {
		return false
	}
	if !slices.Equal(a.pkgChain, b.pkgChain) {
		return false
	}
	return a.unwantedPkg == b.unwantedPkg
}
