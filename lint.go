package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/module"
)

const golangciCfgName = ".golangci.yml"

//go:embed configs/golangci-lint/golangci.yml
var golangciCfgContents []byte

type golangciResult struct {
	Issues []*lintIssue
}

type lintIssue struct {
	FromLinter  string
	Text        string
	SourceLines []string
	Pos         token.Position
}

func (d *depInspector) lintDepVersion(ctx context.Context, dep, version string, pkgs loadedPackages) ([]*lintIssue, error) {
	var golangciLintDirs []string
	var staticcheckDirs []string
	versionStr := makeVersionStr(dep, version)

	if d.inspectAllPkgs || d.unusedDep {
		escPath, err := module.EscapePath(dep)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(d.modCache, escPath)
		golangciLintDirs = []string{fmt.Sprintf("%s@%s%c...", path, version, filepath.Separator)}
		staticcheckDirs = []string{dep + "/..."}
	} else {
		escDep, err := module.EscapePath(dep)
		if err != nil {
			return nil, err
		}
		escVer, err := module.EscapeVersion(version)
		if err != nil {
			return nil, err
		}
		escVerStr := makeVersionStr(escDep, escVer)

		for _, pkg := range pkgs {
			if !strings.HasPrefix(pkg.PkgPath, dep) {
				continue
			}

			pkgPath := strings.TrimPrefix(pkg.PkgPath, dep)
			dir := filepath.Join(d.modCache, escVerStr, pkgPath)

			if !slices.Contains(golangciLintDirs, dir) {
				golangciLintDirs = append(golangciLintDirs, dir)
			}
			if !slices.Contains(staticcheckDirs, pkg.PkgPath) {
				staticcheckDirs = append(staticcheckDirs, pkg.PkgPath)
			}
		}
	}

	var (
		issuesCh = make(chan []*lintIssue, 2)
		errCh    = make(chan error, 2)
		wg       sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()

		log.Printf("linting %s with golangci-lint", versionStr)
		issues, err := d.golangciLint(ctx, golangciLintDirs)
		if err != nil {
			errCh <- fmt.Errorf("linting with golangci-lint: %w", err)
			return
		}
		issuesCh <- issues
	}()
	go func() {
		defer wg.Done()

		log.Printf("linting %s with staticcheck", versionStr)
		issues, err := d.staticcheckLint(ctx, staticcheckDirs)
		if err != nil {
			errCh <- fmt.Errorf("linting with staticcheck: %w", err)
			return
		}
		issuesCh <- issues
	}()

	wg.Wait()
	close(errCh)

	var linterErrs []error
	for err := range errCh {
		linterErrs = append(linterErrs, err)
	}
	if len(linterErrs) != 0 {
		return nil, errors.Join(linterErrs...)
	}

	// sort issues by linter and file
	issues := append(<-issuesCh, <-issuesCh...)
	slices.SortFunc(issues, func(a, b *lintIssue) int {
		if a.FromLinter != b.FromLinter {
			return strings.Compare(a.FromLinter, b.FromLinter)
		}
		if a.Pos.Filename != b.Pos.Filename {
			return strings.Compare(a.Pos.Filename, b.Pos.Filename)
		}
		if a.Pos.Line != b.Pos.Line {
			if a.Pos.Line < b.Pos.Line {
				return -1
			}
			return 1
		}
		if a.Pos.Column != b.Pos.Column {
			if a.Pos.Column < b.Pos.Column {
				return -1
			}
			return 1
		}
		return 0
	})
	for i := range issues {
		filename := issues[i].Pos.Filename
		filename, err := filepath.Abs(filename)
		if err != nil {
			return nil, fmt.Errorf("making path absolute: %w", err)
		}
		issues[i].Pos.Filename, err = trimFilename(filename, d.modCache)
		if err != nil {
			return nil, err
		}

		// make leading whitespace of source code lines uniform
		for j := range issues[i].SourceLines {
			srcLine := issues[i].SourceLines[j]
			srcLine = "\t" + strings.TrimSpace(srcLine)
			issues[i].SourceLines[j] = srcLine
		}
	}

	return issues, nil
}

func (d *depInspector) golangciLint(ctx context.Context, dirs []string) ([]*lintIssue, error) {
	// write embedded golangci-lint config to a temporary file to it can
	// be used by golangci-lint
	cfgDir, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(cfgDir)
	golangciCfgPath := filepath.Join(cfgDir, golangciCfgName)
	if err := os.WriteFile(golangciCfgPath, golangciCfgContents, 0o644); err != nil {
		return nil, fmt.Errorf("writing golangci-lint config file: %w", err)
	}

	var output bytes.Buffer
	cmd := []string{"golangci-lint", "run", "-c", golangciCfgPath, "--out-format=json"}
	cmd = append(cmd, dirs...)
	err = d.runCommand(ctx, &output, cmd...)
	if err != nil {
		// golangci-lint will exit with 1 if any linters returned issues,
		// but that doesn't mean it itself failed
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 1 {
			return nil, err
		}
	}

	var results golangciResult
	if err := json.Unmarshal(output.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("decoding golangci-lint results: %w", err)
	}

	return results.Issues, nil
}

type staticcheckIssue struct {
	Code     string
	Location staticcheckPosition
	End      staticcheckPosition
	Message  string
}

type staticcheckPosition struct {
	File   string
	Line   int
	Column int
}

func (d *depInspector) staticcheckLint(ctx context.Context, dirs []string) ([]*lintIssue, error) {
	var lintBuf bytes.Buffer
	cmd := []string{"staticcheck", "-checks=SA1*,SA2*,SA4*,SA5*,SA9*", "-f=json", "-tests=false"}
	cmd = append(cmd, dirs...)
	err := d.runCommand(ctx, &lintBuf, cmd...)
	if err != nil {
		// staticcheck will exit with 1 if any issues are found, but
		// that doesn't mean it itself failed
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 1 {
			return nil, err
		}
	}

	var sIssues []staticcheckIssue
	dec := json.NewDecoder(&lintBuf)
	for dec.More() {
		var issue staticcheckIssue
		if err := dec.Decode(&issue); err != nil {
			return nil, fmt.Errorf("decoding staticcheck results: %w", err)
		}
		sIssues = append(sIssues, issue)
	}

	issues := make([]*lintIssue, len(sIssues))
	for i, sIssue := range sIssues {
		issue := &lintIssue{
			FromLinter: "staticcheck " + sIssue.Code,
			Text:       trimLinterMsg(sIssue.Message),
			Pos: token.Position{
				Filename: sIssue.Location.File,
				Offset:   sIssue.End.Column, // ?
				Line:     sIssue.Location.Line,
				Column:   sIssue.Location.Column,
			},
		}
		issue.SourceLines, err = getSrcLinesFromFile(
			sIssue.Location.File,
			sIssue.Location.Line,
			sIssue.End.Line,
		)
		if err != nil {
			return nil, err
		}
		issues[i] = issue
	}

	return issues, nil
}

func trimLinterMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg[len(msg)-1] == '.' {
		msg = msg[:len(msg)-1]
	}
	return msg
}

func getSrcLinesFromFile(path string, startLine, endLine int) ([]string, error) {
	srcFile, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening source file: %w", err)
	}
	defer srcFile.Close()
	l := newLineReader(srcFile)

	srcLines, err := getSrcLines(l, startLine, endLine)
	if err != nil {
		return nil, fmt.Errorf("reading source file: %w", err)
	}

	return srcLines, nil
}

func issuesEqual(dep string, a, b *lintIssue) bool {
	if a.FromLinter != b.FromLinter || a.Text != b.Text {
		return false
	}
	if a.Pos.Line != b.Pos.Line {
		return false
	}
	if a.Pos.Column != b.Pos.Column {
		return false
	}
	if len(a.SourceLines) != len(b.SourceLines) {
		return false
	}

	// compare paths after the module version
	filenameA := getDepRelPath(dep, a.Pos.Filename)
	filenameB := getDepRelPath(dep, b.Pos.Filename)
	if filenameA != filenameB {
		return false
	}

	// compare source code lines with leading and trailing whitespace
	// removed; if only whitespace changed between old and new versions
	// the line(s) are semantically the same
	for i := range a.SourceLines {
		srcLineA := strings.TrimSpace(a.SourceLines[i])
		srcLineB := strings.TrimSpace(b.SourceLines[i])
		if srcLineA != srcLineB {
			return false
		}
	}

	return true
}

func getDepRelPath(dep, path string) string {
	depIdx := strings.Index(path, dep)
	if depIdx == -1 {
		return path
	}
	depVerIdx := depIdx + len(dep)
	slashIdx := strings.Index(path[depVerIdx:], "/")
	if slashIdx == -1 {
		log.Printf("could not find slash in path %s", path[depVerIdx:])
		return path
	}

	return path[depVerIdx+slashIdx:]
}

func trimFilename(filename, goModCache string) (string, error) {
	versionDir, file := filepath.Split(filename)
	// trim GOMODCACHE so we just have the escaped package path
	pkgPath := strings.TrimPrefix(versionDir, goModCache+string(filepath.Separator))
	// remove trailing slash if necessary
	if pkgPath[len(pkgPath)-1] == filepath.Separator {
		pkgPath = pkgPath[:len(pkgPath)-1]
	}

	_, verPkg, ok := strings.Cut(pkgPath, "@")
	if !ok {
		return "", fmt.Errorf("cached module dir missing version: %q", filename)
	}
	// if a separator doesn't exist that's fine, the package is the
	// dependency module so only the file will be returned
	_, pkg, _ := strings.Cut(verPkg, string(filepath.Separator))

	return path.Join(pkg, file), nil
}
