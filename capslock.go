package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

//go:embed configs/capslock
var capMaps embed.FS

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

func (d *depInspector) findCapabilities(ctx context.Context, dep, versionStr string, pkgs loadedPackages) ([]capability, error) {
	depPkgs := []string{dep + "/..."}
	var err error
	if !d.inspectAllPkgs {
		depPkgs, err = listImportedPackages(dep, d.modFile.Module.Mod.Path, pkgs)
		if err != nil {
			return nil, err
		}
	}

	// write embedded capability maps to a temporary file to it can
	// be used by capslock
	cfgDir, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(cfgDir)

	capMapFile, err := os.Create(filepath.Join(cfgDir, "dep-inspector.cm"))
	if err != nil {
		return nil, fmt.Errorf("creating temporary file: %w", err)
	}

	err = fs.WalkDir(capMaps, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		f, err := capMaps.Open(path)
		if err != nil {
			return fmt.Errorf("opening embedded capability map: %w", err)
		}
		defer f.Close()

		_, err = io.Copy(capMapFile, f)
		if err != nil {
			return fmt.Errorf("writing to temporary file: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking embedded capability maps: %w", err)
	}
	if err := capMapFile.Close(); err != nil {
		return nil, fmt.Errorf("closing temporary file: %w", err)
	}

	log.Printf("finding capabilities of %s with capslock", versionStr)
	var output bytes.Buffer
	cmd := []string{"capslock", "-packages", strings.Join(depPkgs, ","), "-capability_map", capMapFile.Name(), "-output=json"}
	err = d.runCommand(ctx, &output, cmd...)
	if err != nil {
		return nil, err
	}

	var results capslockResult
	if err := json.Unmarshal(output.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("decoding results from capslock: %w", err)
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
