package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/exp/slices"
	"golang.org/x/mod/modfile"
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

func listPackages(deps bool, pkgs ...string) (packagesInfo, error) {
	var output bytes.Buffer
	cmd := []string{"go", "list", "-json"}
	if deps {
		cmd = append(cmd, "-deps")
	}
	cmd = append(cmd, pkgs...)
	err := runCommand(&output, false, cmd...)
	if err != nil {
		return nil, fmt.Errorf("error listing dependencies: %v", err)
	}

	dec := json.NewDecoder(&output)
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

// TODO: use golang.org/x/tools/go/packages for a speedup
func listImportedPackages(dep string) ([]string, error) {
	var output bytes.Buffer
	err := runCommand(&output, false, "go", "env", "GOMOD")
	if err != nil {
		return nil, fmt.Errorf("error listing imports: %v", err)
	}

	modFilePath := trimNewline(output.String())
	modFileContents, err := os.ReadFile(modFilePath)
	if err != nil {
		return nil, fmt.Errorf("error reading go.mod: %w", err)
	}
	modFile, err := modfile.Parse(modFilePath, modFileContents, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing go.mod: %w", err)
	}
	modName := modFile.Module.Mod.Path

	pkgs, err := listPackages(true, modName+"/...")
	if err != nil {
		return nil, err
	}
	imports := make(map[string][]string)
	for _, pkg := range pkgs {
		if !strings.HasPrefix(pkg.ImportPath, modName) {
			continue
		}

		for _, imp := range pkg.Imports {
			if !strings.HasPrefix(imp, dep) {
				continue
			}

			importedPkg, ok := pkgs[imp]
			if !ok {
				return nil, fmt.Errorf("couldn't find package %s", imp)
			}
			imports[importedPkg.ImportPath] = importedPkg.Imports
		}
	}

	importsToCheck := make([]string, 0, len(imports))
	for pkgPath := range imports {
		addImport := true
		for _, imports := range imports {
			if slices.Contains(imports, pkgPath) {
				addImport = false
				break
			}
		}
		if addImport {
			importsToCheck = append(importsToCheck, pkgPath)
		}
	}

	return importsToCheck, nil
}

func trimNewline(s string) string {
	if len(s) != 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
