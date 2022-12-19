package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"golang.org/x/exp/slices"
)

var unwantedPkgs = []string{
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

type usedPackages map[string]*listedPackage

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
	Path string
}

func listPackages(pkgs ...string) (usedPackages, error) {
	var listBuf bytes.Buffer
	cmd := []string{"go", "list", "-deps", "-json"}
	cmd = append(cmd, pkgs...)
	err := runCommand(&listBuf, false, cmd...)
	if err != nil {
		return nil, fmt.Errorf("error listing dependencies: %v", err)
	}

	dec := json.NewDecoder(&listBuf)
	listedPkgs := make(usedPackages)
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

func findUnwantedImports(dep string, pkgs usedPackages) ([]packageIssue, error) {
	var depPkgs []string
	for _, pkg := range pkgs {
		if !pkg.Standard && pkg.Module.Path == dep {
			depPkgs = append(depPkgs, pkg.ImportPath)
		}
	}
	pkgs, err := listPackages(depPkgs...)
	if err != nil {
		return nil, err
	}

	var pkgIssues []packageIssue
	for _, p := range depPkgs {
		pkg, ok := pkgs[p]
		if !ok {
			return nil, fmt.Errorf("could not find package %s", p)
		}
		foundImps := findIn(pkg.Imports, unwantedPkgs)
		for _, f := range foundImps {
			pkgIssues = append(pkgIssues, packageIssue{
				srcPkg:      p,
				unwantedPkg: f,
			})
		}
		// TODO: always do this?
		if len(foundImps) != 0 {
			continue
		}

		// foundDeps := findIn(pkg.Deps, unwantedPkgs)
		// for _, f := range foundDeps {
		// }
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
