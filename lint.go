package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const golangciCfgName = ".golangci.yml"

//go:embed configs/golangci-lint/golangci.yml
var golangciCfgContents []byte

type golangciResult struct {
	Issues []lintIssue
}

type lintIssue struct {
	FromLinter  string
	Text        string
	SourceLines []string
	Pos         token.Position
}

func lintDepVersion(dep, versionStr string, modName string, pkgs loadedPackages) ([]lintIssue, error) {
	var dirs []string
	for _, pkg := range pkgs {
		if pkg.Module == nil || !strings.HasPrefix(pkg.Module.Path, dep) {
			continue
		}

		dir := filepath.Dir(pkg.GoFiles[0])
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}

	log.Printf("linting %s with golangci-lint", versionStr)
	golangciIssues, err := golangciLint(dep, dirs)
	if err != nil {
		return nil, fmt.Errorf("error linting %s with golangci-lint: %v", versionStr, err)
	}

	log.Printf("linting %s with staticcheck", versionStr)
	staticcheckIssues, err := staticcheckLint(dep, dirs)
	if err != nil {
		return nil, fmt.Errorf("error linting %s with staticcheck: %v", versionStr, err)
	}

	// sort issues by linter and file
	issues := append(golangciIssues, staticcheckIssues...)
	slices.SortFunc(issues, func(a, b lintIssue) int {
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
	// make leading whitespace of source code lines uniform
	for i := range issues {
		for j := range issues[i].SourceLines {
			srcLine := issues[i].SourceLines[j]
			srcLine = "\t" + strings.TrimSpace(srcLine)
			issues[i].SourceLines[j] = srcLine
		}
	}

	return issues, nil
}

func golangciLint(dep string, dirs []string) ([]lintIssue, error) {
	// write embedded golangci-lint config to a temporary file to it can
	// be used later
	cfgDir, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, fmt.Errorf("error creating temporary file: %v", err)
	}
	defer os.RemoveAll(cfgDir)
	golangciCfgPath := filepath.Join(cfgDir, golangciCfgName)
	if err := os.WriteFile(golangciCfgPath, golangciCfgContents, 0o644); err != nil {
		return nil, fmt.Errorf("error writing golangci-lint config file: %v", err)
	}

	var output bytes.Buffer
	cmd := []string{"golangci-lint", "run", "-c", golangciCfgPath, "--out-format=json"}
	cmd = append(cmd, dirs...)
	err = runCommand(&output, false, cmd...)
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
		return nil, fmt.Errorf("error decoding: %v", err)
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

func staticcheckLint(dep string, dirs []string) ([]lintIssue, error) {
	var lintBuf bytes.Buffer
	cmd := []string{"staticcheck", "-checks=SA1*,SA2*,SA4*,SA5*,SA9*", "-f=json", "-tests=false"}
	cmd = append(cmd, dirs...)
	err := runCommand(&lintBuf, false, cmd...)
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
			return nil, fmt.Errorf("error decoding: %v", err)
		}
		sIssues = append(sIssues, issue)
	}

	issues := make([]lintIssue, len(sIssues))
	for i, sIssue := range sIssues {
		issue := lintIssue{
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
		return nil, fmt.Errorf("error opening source file: %v", err)
	}
	defer srcFile.Close()
	l := newLineReader(srcFile)

	srcLines, err := getSrcLines(l, startLine, endLine)
	if err != nil {
		return nil, fmt.Errorf("error reading source file: %v", err)
	}

	return srcLines, nil
}

func issuesEqual(dep string, a, b lintIssue) bool {
	if a.FromLinter != b.FromLinter || a.Text != b.Text {
		return false
	}
	if a.Pos.Line != b.Pos.Line {
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
		log.Printf("could not find %s in path %s", dep, path)
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

func makeVersionStr(dep, version string) string {
	return dep + "@" + version
}
