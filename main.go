package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
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
	// TODO: add flags for:
	// - lint tests
	// - merge golangci-lint config
	// - specify staticcheck checks
	// - build tags
	flag.StringVar(&logPath, "l", "stdout", "path to log to")
	flag.BoolVar(&verbose, "v", false, "print commands being run and verbose information")
	flag.BoolVar(&printVersion, "version", false, "print version and build information and exit")
}

func main() {
	os.Exit(mainRetCode())
}

/*
TODO:

- inspect changed indirect deps as well

- check if CGO is used
- check if unsafe is imported
  - details on linkname directives
- check if embed is imported
  - check size diff of embedded files
  - try and determine type of file
    - https://pkg.go.dev/github.com/h2non/filetype#Match
	- only need first 262 bytes of file
- check binary diff of with updated dep(s)?
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

	if narg := flag.NArg(); narg != 1 && narg != 3 {
		usage()
		return 2
	}

	goModCache, err := getGoModCache()
	if err != nil {
		log.Println(err)
		return 1
	}

	if flag.NArg() == 1 {
		depVer := flag.Arg(0)
		dep, ver, ok := strings.Cut(depVer, "@")
		if !ok {
			log.Println(`malformed version string: no "@" present`)
			usage()
			return 2
		}

		lintIssues, pkgIssues, err := inspectDep(dep, ver, goModCache)
		if err != nil {
			log.Println(err)
			return 1
		}

		printLinterIssues(lintIssues, goModCache)
		printPkgIssues(pkgIssues, goModCache)
		return 0
	}

	dep := flag.Arg(0)
	oldVer := flag.Arg(1)
	newVer := flag.Arg(2)
	results, err := inspectDepVersions(dep, oldVer, newVer, goModCache)
	if err != nil {
		log.Println(err)
		return 1
	}

	// print linter issues
	if len(results.fixedIssues) > 0 {
		fmt.Println("fixed issues:")
		printLinterIssues(results.fixedIssues, goModCache)
	}
	if len(results.staleIssues) > 0 {
		fmt.Println("stale issues:")
		printLinterIssues(results.staleIssues, goModCache)
	}
	if len(results.newIssues) > 0 {
		fmt.Println("new issues:")
		printLinterIssues(results.newIssues, goModCache)
	}
	fmt.Printf("total:\nfixed issues: %d\nstale issues: %d\nnew issues:   %d\n\n",
		len(results.fixedIssues),
		len(results.staleIssues),
		len(results.newIssues),
	)

	// print package issues
	if len(results.removedPkgs) > 0 {
		fmt.Println("removed unwanted packages:")
		printPkgIssues(results.removedPkgs, goModCache)
	}
	if len(results.stalePkgs) > 0 {
		fmt.Println("stale unwanted packages:")
		printPkgIssues(results.stalePkgs, goModCache)
	}
	if len(results.addedPkgs) > 0 {
		fmt.Println("added unwanted packages:")
		printPkgIssues(results.addedPkgs, goModCache)
	}
	fmt.Printf("total:\nremoved unwanted packages: %d\nstale unwanted packages:   %d\nadded unwanted packages:   %d\n",
		len(results.removedPkgs),
		len(results.stalePkgs),
		len(results.addedPkgs),
	)

	return 0
}

func inspectDep(dep, version, goModCache string) ([]lintIssue, []packageIssue, error) {
	pkgs, err := setupDepVersion(makeVersionStr(dep, version))
	if err != nil {
		return nil, nil, err
	}
	pkgIssues, err := findUnwantedImports(dep, pkgs)
	if err != nil {
		return nil, nil, err
	}
	versionStr := makeVersionStr(dep, version)
	lintIssues, err := lintDepVersion(goModCache, dep, versionStr, pkgs)
	if err != nil {
		return nil, nil, err
	}

	return lintIssues, pkgIssues, err
}

type inspectResults struct {
	fixedIssues []lintIssue
	staleIssues []lintIssue
	newIssues   []lintIssue

	removedPkgs []packageIssue
	stalePkgs   []packageIssue
	addedPkgs   []packageIssue
}

func inspectDepVersions(dep, oldVer, newVer, goModCache string) (*inspectResults, error) {
	// inspect old version
	oldVerStr := makeVersionStr(dep, oldVer)
	oldPkgs, err := setupDepVersion(oldVerStr)
	if err != nil {
		return nil, err
	}
	oldPkgIssues, err := findUnwantedImports(dep, oldPkgs)
	if err != nil {
		return nil, err
	}
	oldLintIssues, err := lintDepVersion(goModCache, dep, oldVerStr, oldPkgs)
	if err != nil {
		return nil, err
	}

	// inspect new version
	newVerStr := makeVersionStr(dep, newVer)
	newPkgs, err := setupDepVersion(newVerStr)
	if err != nil {
		return nil, err
	}
	newPkgIssues, err := findUnwantedImports(dep, newPkgs)
	if err != nil {
		return nil, err
	}
	newLintIssues, err := lintDepVersion(goModCache, dep, newVerStr, newPkgs)
	if err != nil {
		return nil, err
	}

	// process linter and package issues
	fixedIssues, staleIssues, newIssues := processIssues(oldLintIssues, newLintIssues, func(a, b lintIssue) bool {
		return issuesEqual(dep, a, b)
	})
	removedPkgs, stalePkgs, addedPkgs := processIssues(oldPkgIssues, newPkgIssues, pkgIssuesEqual)

	return &inspectResults{
		fixedIssues: fixedIssues,
		staleIssues: staleIssues,
		newIssues:   newIssues,
		removedPkgs: removedPkgs,
		stalePkgs:   stalePkgs,
		addedPkgs:   addedPkgs,
	}, nil
}

func getGoModCache() (string, error) {
	var sb strings.Builder
	err := runCommand(&sb, false, "go", "env", "GOMODCACHE")
	if err != nil {
		return "", fmt.Errorf("error getting GOMODCACHE: %v", err)
	}
	// 'go env' output always ends with a newline
	if sb.Len() < 2 {
		return "", errors.New("GOMODCACHE is empty")
	}

	return sb.String()[:sb.Len()-1], nil
}

func setupDepVersion(versionStr string) (packagesInfo, error) {
	// add dep to go.mod so linting it will work
	err := runGoCommand("go", "get", versionStr)
	if err != nil {
		return nil, fmt.Errorf("error downloading %q: %v", versionStr, err)
	}
	err = runGoCommand("go", "mod", "tidy")
	if err != nil {
		return nil, fmt.Errorf("error tidying modules: %v", err)
	}

	pkgs, err := listPackages()
	if err != nil {
		return nil, err
	}

	return pkgs, nil
}

func processIssues[T any](oldVerIssues, newVerIssues []T, equal func(a, b T) bool) ([]T, []T, []T) {
	var (
		fixedIssues []T
		staleIssues []T
		newIssues   []T
	)

	for _, issue := range oldVerIssues {
		idx := slices.IndexFunc(newVerIssues, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			fixedIssues = append(fixedIssues, issue)
		} else {
			staleIssues = append(staleIssues, newVerIssues[idx])
		}
	}
	for _, issue := range newVerIssues {
		idx := slices.IndexFunc(oldVerIssues, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			newIssues = append(newIssues, issue)
		}
	}

	return fixedIssues, staleIssues, newIssues
}

func printLinterIssues(issues []lintIssue, goModCache string) {
	for _, issue := range issues {
		filename := trimFilename(issue.Pos.Filename, goModCache)
		srcLines := strings.Join(issue.SourceLines, "\n")

		fmt.Printf("(%s) %s: %s:%d:%d:\n%s\n\n", issue.FromLinter, issue.Text, filename, issue.Pos.Line, issue.Pos.Column, srcLines)
	}
}

func printPkgIssues(issues []packageIssue, goModCache string) {
	for _, issue := range issues {
		fmt.Printf("%s imports %s\n", issue.srcPkg, issue.unwantedPkg)
		if len(issue.pkgChain) > 0 {
			fmt.Println("import chain:")
			for _, pkg := range issue.pkgChain {
				fmt.Println(pkg)
			}
		}
		fmt.Println("calls:")
		for _, call := range issue.calls {
			posStr := trimFilename(call.position.String(), goModCache)
			srcLines := strings.Join(call.sourceLines, "\n")

			fmt.Printf("%s:\n%s\n\n", posStr, srcLines)
		}
		fmt.Println()
	}
}

func trimFilename(path, goModCache string) string {
	return strings.TrimPrefix(path, goModCache+string(filepath.Separator))
}
