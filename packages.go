package main

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/exp/maps"
	"golang.org/x/tools/go/packages"
)

type loadedPackages map[string]*packages.Package

func listPackages(modName string) (loadedPackages, error) {
	mode := packages.NeedName | packages.NeedImports | packages.NeedDeps | packages.NeedModule | packages.NeedEmbedFiles
	cfg := &packages.Config{Mode: mode}
	pkgs, err := packages.Load(cfg, modName+"/...")
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}
	loadedPkgs := make(loadedPackages)
	mapLoadedPkgs(pkgs, loadedPkgs)

	return loadedPkgs, nil
}

func mapLoadedPkgs(pkgs []*packages.Package, loadedPkgs loadedPackages) {
	for _, pkg := range pkgs {
		if _, ok := loadedPkgs[pkg.PkgPath]; ok {
			continue
		}

		loadedPkgs[pkg.PkgPath] = pkg
		mapLoadedPkgs(maps.Values(pkg.Imports), loadedPkgs)
	}
}

func listImportedPackages(dep string, pkgs loadedPackages) ([]string, error) {
	pkgImports := make(map[string][]string)

	for _, pkg := range pkgs {
		if !strings.HasPrefix(pkg.PkgPath, dep) {
			continue
		}

		importedPkg, ok := pkgs[pkg.PkgPath]
		if !ok {
			return nil, fmt.Errorf("couldn't find package %s", pkg)
		}
		pkgImports[importedPkg.PkgPath] = maps.Keys(importedPkg.Imports)
	}

	// don't add a package to be checked if another package imports it,
	// it will be recursively checked already so explicitly specifying
	// it will lead to duplicate or inaccurate results from capslock
	importsToCheck := make([]string, 0, len(pkgImports))
	for pkgPath := range pkgImports {
		addImport := true
		for _, imports := range pkgImports {
			if slices.Contains(imports, pkgPath) {
				addImport = false
				break
			}
		}
		if addImport {
			importsToCheck = append(importsToCheck, pkgPath)
		}
	}

	if len(importsToCheck) == 0 {
		return nil, fmt.Errorf("there are no packages to check with capslock")
	}

	return importsToCheck, nil
}
