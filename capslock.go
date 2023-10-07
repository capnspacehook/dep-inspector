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
	"slices"
	"strings"
)

//go:embed configs/capslock
var capMaps embed.FS

type capslockResult struct {
	CapabilityInfo []*capability
	ModuleInfo     []capModule
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

type capModule struct {
	Path    string
	Version string
}

func (d *depInspector) findCapabilities(ctx context.Context, dep, versionStr string, pkgs loadedPackages) (*capslockResult, error) {
	allPkgs := dep + "/..."
	var depPkgs []string
	if d.inspectAllPkgs || d.unusedDep {
		depPkgs = []string{allPkgs}
	} else {
		pkgs, err := listImportedPackages(dep, d.parsedModFile.Module.Mod.Path, pkgs)
		if err != nil {
			return nil, err
		}
		depPkgs = pkgs
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
	results.CapabilityInfo = slices.Clip(results.CapabilityInfo)
	slices.SortFunc(results.CapabilityInfo, compareCaps)

	return &results, nil
}

func compareCaps(a, b *capability) int {
	if len(a.Path) != len(b.Path) {
		if len(a.Path) < len(b.Path) {
			return -1
		}
		return 1
	}
	if a.Capability != b.Capability {
		return strings.Compare(a.Capability, b.Capability)
	}
	if a.PackageDir != b.PackageDir {
		return strings.Compare(a.PackageDir, b.PackageDir)
	}
	if a.CapabilityType != b.CapabilityType {
		return strings.Compare(a.CapabilityType, b.CapabilityType)
	}

	for i := range a.Path {
		if a.Path[i].Name != b.Path[i].Name {
			return strings.Compare(a.Path[i].Name, b.Path[i].Name)
		}
		if a.Path[i].Site.Filename != b.Path[i].Site.Filename {
			return strings.Compare(a.Path[i].Site.Filename, b.Path[i].Site.Filename)
		}
		if a.Path[i].Site.Line != b.Path[i].Site.Line {
			if a.Path[i].Site.Line < b.Path[i].Site.Line {
				return -1
			}
			return 1
		}
		if a.Path[i].Site.Column != b.Path[i].Site.Column {
			if a.Path[i].Site.Column < b.Path[i].Site.Column {
				return -1
			}
			return 1
		}
	}

	return 0
}

func capsEqual(a, b *capability) bool {
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
