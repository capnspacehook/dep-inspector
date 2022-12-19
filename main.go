package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"golang.org/x/exp/slices"
)

const (
	projectName = "Dep Inspector"

	modName         = "dep-inspector"
	golangciCfgName = ".golangci.yml"
)

var (
	logPath      string
	verbose      bool
	printVersion bool

	//go:embed .golangci-deps.yml
	golangciCfgContents []byte

	goEnvVars = []string{
		"HOME",
		"PATH",
	}
)

func usage() {
	fmt.Fprintf(os.Stderr, `
<Project description>

	<binary name> [flags]

<Project details/usage>

%s accepts the following flags:

`[1:], projectName)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `

For more information, see https://github.com/capnspacehook/dep-inspector.
`[1:])
}

func init() {
	flag.Usage = usage
	flag.StringVar(&logPath, "l", "stdout", "path to log to")
	flag.BoolVar(&verbose, "v", false, "print commands being run and verbose information")
	flag.BoolVar(&printVersion, "version", false, "print version and build information and exit")
}

func main() {
	os.Exit(mainRetCode())
}

/*
TODO:

- ignore main pkg dirs
- check if CGO is used
- check if unsafe is imported
  - details on linkname directives
- check if os/signal is imported and signal handlers modified
- check if os/exec is imported and used
- check if runtime is used in a way that will affect package main
  - Breakpoint
  - GC
  - GOMAXPROCS
  - (Lock/Unlock)OSThread
  - MemProfile
  - MutexProfile
  - ReadMemStats
  - ReadTrace
  - SetBlockProfileRate
  - SetCPUProfileRate
  - SetCgoTraceback
  - SetFinalizer
  - SetMutexProfileFraction
  - (Start/Stop)Trace
  - ThreadCreateProfile
- check if runtime/debug is imported
- check if runtime/metrics is imported
- check if runtime/pprof is imported
- check if runtime/trace is imported
- check if reflect is imported
- check if embed is imported
  - check size diff of embedded files
  - try and determine type of file
    - https://pkg.go.dev/github.com/h2non/filetype#Match
	- only need first 262 bytes of file
- check binary diff of with updated dep(s)

go list -json -deps

find packages that import specific packages
*/

func mainRetCode() int {
	flag.Parse()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		log.Println("build information not found")
		return 1
	}
	if printVersion {
		printVersionInfo(info)
		return 0
	}

	if flag.NArg() != 3 {
		usage()
		return 2
	}

	if err := lintDep(flag.Arg(0), flag.Arg(1), flag.Arg(2)); err != nil {
		log.Println(err)
		return 1
	}

	return 0
}

func lintDep(dep, oldVer, newVer string) error {
	// find GOMODCACHE
	var sb strings.Builder
	err := runCommand(&sb, false, ".", "go", "env", "GOMODCACHE")
	if err != nil {
		return fmt.Errorf("error getting GOMODCACHE: %v", err)
	}
	// 'go env' output always ends with a newline
	if sb.Len() < 2 {
		return errors.New("GOMODCACHE is empty")
	}
	goModCache := sb.String()[:sb.Len()-1]

	oldVerResults, err := lintDepVersion(".", goModCache, dep, oldVer)
	if err != nil {
		return err
	}
	newVerResults, err := lintDepVersion(".", goModCache, dep, newVer)
	if err != nil {
		return err
	}

	var (
		fixedIssues []lintIssue
		staleIssues []lintIssue
		newIssues   []lintIssue
	)

	for _, issue := range oldVerResults {
		idx := slices.IndexFunc(newVerResults, func(li lintIssue) bool {
			return issuesEqual(dep, li, issue)
		})
		if idx == -1 {
			fixedIssues = append(fixedIssues, issue)
			continue
		}
		staleIssues = append(staleIssues, newVerResults[idx])
	}
	for _, issue := range newVerResults {
		idx := slices.IndexFunc(oldVerResults, func(li lintIssue) bool {
			return issuesEqual(dep, li, issue)
		})
		if idx != -1 {
			continue
		}
		newIssues = append(newIssues, issue)
	}

	if len(fixedIssues) > 0 {
		fmt.Println("fixed issues:")
		printIssues(fixedIssues, makeVersionStr(dep, oldVer))
	}
	if len(staleIssues) > 0 {
		fmt.Println("stale issues:")
		printIssues(staleIssues, makeVersionStr(dep, newVer))
	}
	if len(newIssues) > 0 {
		fmt.Println("new issues:")
		printIssues(newIssues, makeVersionStr(dep, newVer))
	}

	fmt.Printf("total:\nfixed issues: %d\nstale issues: %d\nnew issues:   %d\n",
		len(fixedIssues),
		len(staleIssues),
		len(newIssues),
	)

	return nil
}

type lintResult struct {
	Issues []lintIssue
}

type lintIssue struct {
	FromLinter  string
	Text        string
	SourceLines []string
	Pos         lintPosition
}

type lintPosition struct {
	Filename string
	Offset   int
	Line     int
	Column   int
}

func lintDepVersion(dir, goModCache, dep, version string) ([]lintIssue, error) {
	// add dep to go.mod so linting it will work
	versionStr := makeVersionStr(dep, version)
	err := runGoCommand(dir, "go", "get", versionStr)
	if err != nil {
		return nil, fmt.Errorf("error downloading %q: %v", versionStr, err)
	}
	err = runGoCommand(dir, "go", "mod", "tidy")
	if err != nil {
		return nil, fmt.Errorf("error tidying modules: %v", err)
	}

	pkgs, err := listPackage(".", "")
	if err != nil {
		return nil, err
	}

	log.Printf("linting %s with golangci-lint", versionStr)
	golangciIssues, err := golangciLint(dir, dep, pkgs)
	if err != nil {
		return nil, fmt.Errorf("error linting %s with golangci-lint: %v", versionStr, err)
	}

	// TODO: for each package, find list of Go files for that version
	// and pass the filenames to staticcheck
	log.Printf("linting %s with staticcheck", versionStr)
	staticcheckIssues, err := staticcheckLint(dir, dep, pkgs)
	if err != nil {
		return nil, fmt.Errorf("error linting %s with staticcheck: %v", versionStr, err)
	}

	issues := append(golangciIssues, staticcheckIssues...)
	slices.SortFunc(issues, func(a, b lintIssue) bool {
		if a.FromLinter != b.FromLinter {
			return a.FromLinter < b.FromLinter
		}
		if a.Pos.Filename != b.Pos.Filename {
			return a.Pos.Filename < b.Pos.Filename
		}
		if a.Pos.Line != b.Pos.Line {
			return a.Pos.Line < b.Pos.Line
		}
		return a.Pos.Column < b.Pos.Column
	})

	return issues, nil
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

func listPackage(dir string, pkg string) (usedPackages, error) {
	var listBuf bytes.Buffer

	if pkg != "" {
		err := runCommand(&listBuf, false, dir, "go", "list", "-deps", "-json", pkg)
		if err != nil {
			return nil, fmt.Errorf("error listing dependencies of %s: %v", pkg, err)
		}
	} else {
		err := runCommand(&listBuf, false, dir, "go", "list", "-deps", "-json")
		if err != nil {
			return nil, fmt.Errorf("error listing dependencies: %v", err)
		}
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

func golangciLint(dir, dep string, pkgs usedPackages) ([]lintIssue, error) {
	var dirs []string
	for _, pkg := range pkgs {
		if !pkg.Standard && pkg.Module.Path == dep {
			dirs = append(dirs, pkg.Dir)
		}
	}

	// write embedded golangci-lint config to a temporary file to it can
	// be used later
	cfgDir, err := os.MkdirTemp("", modName)
	if err != nil {
		return nil, fmt.Errorf("error creating temporary file: %v", err)
	}
	defer os.RemoveAll(cfgDir)
	golangciCfgPath := filepath.Join(cfgDir, golangciCfgName)
	if err := os.WriteFile(golangciCfgPath, golangciCfgContents, 0o644); err != nil {
		return nil, fmt.Errorf("error writing golangci-lint config file: %v", err)
	}

	var lintBuf bytes.Buffer
	cmd := []string{"golangci-lint", "run", "-c", golangciCfgPath, "--out-format=json"}
	cmd = append(cmd, dirs...)
	err = runCommand(&lintBuf, false, dir, cmd...)
	if err != nil {
		// golangci-lint will exit with 1 if any linters returned issues,
		// but that doesn't mean it itself failed
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 1 {
			return nil, err
		}
	}

	var results lintResult
	if err := json.Unmarshal(lintBuf.Bytes(), &results); err != nil {
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

func staticcheckLint(dir, dep string, pkgs usedPackages) ([]lintIssue, error) {
	var dirs []string
	for _, pkg := range pkgs {
		if !pkg.Standard && pkg.Module.Path == dep {
			dirs = append(dirs, pkg.Dir)
		}
	}

	var lintBuf bytes.Buffer
	cmd := []string{"staticcheck", "-checks=SA1*,SA2*,SA4*,SA5*,SA9*", "-f=json", "-tests=false"}
	cmd = append(cmd, dirs...)
	err := runCommand(&lintBuf, false, dir, cmd...)
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
			FromLinter: "staticcheck",
			Text:       fmt.Sprintf("%s: %s", sIssue.Code, sIssue.Message),
			Pos: lintPosition{
				Filename: sIssue.Location.File,
				Offset:   sIssue.End.Column, // ?
				Line:     sIssue.Location.Line,
				Column:   sIssue.Location.Column,
			},
		}
		issue.SourceLines, err = getSrcLines(sIssue)
		if err != nil {
			return nil, err
		}
		issues[i] = issue
	}

	return issues, nil
}

func getSrcLines(sIssue staticcheckIssue) ([]string, error) {
	srcFile, err := os.Open(sIssue.Location.File)
	if err != nil {
		return nil, fmt.Errorf("error opening source file %s: %v", sIssue.Location.File, err)
	}
	defer srcFile.Close()
	s := bufio.NewScanner(srcFile)

	line := 1
	srcLines := make([]string, 0, 1)
	for s.Scan() {
		if line == sIssue.Location.Line {
			srcLines = append(srcLines, s.Text())
		}
		if line == sIssue.End.Line {
			if sIssue.Location.Line != sIssue.End.Line {
				srcLines = append(srcLines, s.Text())
			}
			break
		}
		if line > sIssue.Location.Line && line < sIssue.End.Line {
			srcLines = append(srcLines, s.Text())
		}
		line++
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("error reading source file %s: %v", sIssue.Location.File, err)
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

func printIssues(issues []lintIssue, versionStr string) {
	for _, issue := range issues {
		filename := issue.Pos.Filename
		idx := strings.Index(issue.Pos.Filename, versionStr)
		if idx == -1 {
			log.Printf("malformed filename: %q", filename)
		} else {
			filename = filename[idx:]
		}
		srcLines := strings.Join(issue.SourceLines, "\n")

		fmt.Printf("(%s) %s: %s:%d:%d:\n%s\n\n", issue.FromLinter, issue.Text, filename, issue.Pos.Line, issue.Pos.Column, srcLines)
	}
}

func runGoCommand(dir string, args ...string) error {
	var writer io.Writer
	if verbose {
		writer = os.Stderr
	}

	env := make([]string, len(goEnvVars))
	for _, envVar := range goEnvVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar, os.Getenv(envVar)))
	}

	return buildCommand(writer, true, dir, env, args...).Run()
}

func runCommand(writer io.Writer, stderr bool, dir string, args ...string) error {
	return buildCommand(writer, stderr, dir, nil, args...).Run()
}

func buildCommand(writer io.Writer, stderr bool, dir string, env []string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(args) == 1 {
		cmd = exec.Command(args[0])
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}

	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = writer
	if stderr {
		cmd.Stderr = writer
	}

	if verbose {
		log.Printf("running command: %q", cmd)
	}

	return cmd
}

func makeVersionStr(dep, version string) string {
	return dep + "@" + version
}
