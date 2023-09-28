package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
