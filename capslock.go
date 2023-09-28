package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

type capslockResult struct {
	CapabilityInfo []capability
}

type capability struct {
	PackageName    string
	Capability     string
	Path           []functionCall
	PackageDir     string
	CapabilityType string
}

type functionCall struct {
	Name string
	Site callSite
}

type callSite struct {
	Filename string
	Line     string
	Column   string
}

func findCapabilities(dep, versionStr string, pkgs packagesInfo) ([]capability, error) {
	var seenPkg bool
	var depPkgs strings.Builder
	for importPath := range pkgs {
		if strings.HasPrefix(importPath, dep) {
			if seenPkg {
				depPkgs.WriteRune(',')
			}
			seenPkg = true
			depPkgs.WriteString(importPath)
		}
	}

	log.Printf("finding capabilities of %s with capslock", versionStr)
	var output bytes.Buffer
	cmd := []string{"capslock", "-packages", depPkgs.String(), "-output=json"}
	err := runCommand(&output, false, cmd...)
	if err != nil {
		return nil, err
	}

	var results capslockResult
	if err := json.Unmarshal(output.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("error decoding: %v", err)
	}
	caps := results.CapabilityInfo

	return caps, nil
}

func capsEqual(a, b capability) bool {
	if a.PackageDir != b.PackageDir {
		return false
	}
	if a.PackageName != b.PackageName {
		return false
	}
	if a.Capability != b.Capability {
		return false
	}
	if a.CapabilityType != b.CapabilityType {
		return false
	}
	if len(a.Path) != len(b.Path) {
		return false
	}

	for i := range a.Path {
		if a.Path[i].Name != b.Path[i].Name {
			return false
		}

		callA := a.Path[i].Site
		callB := a.Path[i].Site
		if callA.Filename != callB.Filename {
			return false
		}
		if callA.Line != callB.Line {
			return false
		}
		if callA.Column != callB.Column {
			return false
		}
	}

	return true
}
