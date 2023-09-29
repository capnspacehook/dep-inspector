package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/pkg/browser"
)

const (
	projectName = "Dep Inspector"

	tempPrefix = "dep-inspector"
)

var (
	allPkgs      bool
	htmlOutput   bool
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
	flag.BoolVar(&allPkgs, "a", false, "inspect all packages of the dependency, not just those that are used")
	flag.BoolVar(&htmlOutput, "html", false, "output findings in html")
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
			// TODO: support not passing version and just using what's in go.mod
			log.Println(`malformed version string: no "@" present`)
			usage()
			return 2
		}

		modFile, err := parseGoMod()
		if err != nil {
			log.Println(err)
			return 1
		}
		modName := modFile.Module.Mod.Path
		caps, lintIssues, err := inspectDep(allPkgs, modName, dep, ver, goModCache)
		if err != nil {
			log.Println(err)
			return 1
		}

		if htmlOutput {
			r, err := formatHTMLOutput(dep, ver, caps, lintIssues)
			if err != nil {
				log.Println(err)
				return 1
			}

			err = browser.OpenReader(r)
			if err != nil {
				log.Println(err)
				return 1
			}

			return 0
		}

		printCaps(caps)
		printLinterIssues(lintIssues)
		return 0
	}

	dep := flag.Arg(0)
	oldVer := flag.Arg(1)
	newVer := flag.Arg(2)
	results, err := inspectDepVersions(allPkgs, dep, oldVer, newVer, goModCache)
	if err != nil {
		log.Println(err)
		return 1
	}

	// print package issues
	if len(results.removedCaps) > 0 {
		fmt.Println("removed capabilities:")
		printCaps(results.removedCaps)
	}
	if len(results.staleCaps) > 0 {
		fmt.Println("stale capabilities:")
		printCaps(results.staleCaps)
	}
	if len(results.addedCaps) > 0 {
		fmt.Println("added capabilities:")
		printCaps(results.addedCaps)
	}
	fmt.Printf("total:\nremoved capabilities: %d\nstale capabilities:   %d\nadded capabilities:   %d\n",
		len(results.removedCaps),
		len(results.staleCaps),
		len(results.addedCaps),
	)

	// print linter issues
	if len(results.fixedIssues) > 0 {
		fmt.Println("fixed issues:")
		printLinterIssues(results.fixedIssues)
	}
	if len(results.staleIssues) > 0 {
		fmt.Println("stale issues:")
		printLinterIssues(results.staleIssues)
	}
	if len(results.newIssues) > 0 {
		fmt.Println("new issues:")
		printLinterIssues(results.newIssues)
	}
	fmt.Printf("total:\nfixed issues: %d\nstale issues: %d\nnew issues:   %d\n\n",
		len(results.fixedIssues),
		len(results.staleIssues),
		len(results.newIssues),
	)

	return 0
}

func inspectDep(allPkgs bool, modName, dep, version, goModCache string) ([]capability, []lintIssue, error) {
	versionStr := makeVersionStr(dep, version)
	if err := setupDepVersion(versionStr); err != nil {
		return nil, nil, fmt.Errorf("setting up dependency: %w", err)
	}

	pkgs, err := listPackages(modName)
	if err != nil {
		return nil, nil, err
	}

	caps, err := findCapabilities(allPkgs, dep, versionStr, modName, pkgs)
	if err != nil {
		return nil, nil, fmt.Errorf("finding capabilities of dependency: %w", err)
	}
	lintIssues, err := lintDepVersion(allPkgs, dep, versionStr, pkgs, goModCache)
	if err != nil {
		return nil, nil, fmt.Errorf("linting dependency: %w", err)
	}

	return caps, lintIssues, err
}

type inspectResults struct {
	fixedIssues []lintIssue
	staleIssues []lintIssue
	newIssues   []lintIssue

	removedCaps []capability
	staleCaps   []capability
	addedCaps   []capability
}

func inspectDepVersions(allPkgs bool, dep, oldVer, newVer, goModCache string) (*inspectResults, error) {
	modFile, err := parseGoMod()
	if err != nil {
		return nil, err
	}
	modName := modFile.Module.Mod.Path

	// inspect old version
	oldCaps, oldLintIssues, err := inspectDep(allPkgs, modName, dep, oldVer, goModCache)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", makeVersionStr(dep, oldVer), err)
	}

	// inspect new version
	newCaps, newLintIssues, err := inspectDep(allPkgs, modName, dep, newVer, goModCache)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", makeVersionStr(dep, newVer), err)
	}

	// process linter issues and capabilities
	fixedIssues, staleIssues, newIssues := processFindings(oldLintIssues, newLintIssues, func(a, b lintIssue) bool {
		return issuesEqual(dep, a, b)
	})
	removedCaps, staleCaps, addedCaps := processFindings(oldCaps, newCaps, capsEqual)

	return &inspectResults{
		fixedIssues: fixedIssues,
		staleIssues: staleIssues,
		newIssues:   newIssues,
		removedCaps: removedCaps,
		staleCaps:   staleCaps,
		addedCaps:   addedCaps,
	}, nil
}

func getGoModCache() (string, error) {
	var sb strings.Builder
	err := runCommand(&sb, false, "go", "env", "GOMODCACHE")
	if err != nil {
		return "", fmt.Errorf("getting GOMODCACHE: %w", err)
	}
	// 'go env' output always ends with a newline
	if sb.Len() < 2 {
		return "", errors.New("GOMODCACHE is empty")
	}

	return sb.String()[:sb.Len()-1], nil
}

func setupDepVersion(versionStr string) error {
	// add dep to go.mod so linting it will work
	err := runGoCommand("go", "get", versionStr)
	if err != nil {
		return fmt.Errorf("downloading %q: %w", versionStr, err)
	}
	err = runGoCommand("go", "mod", "tidy")
	if err != nil {
		return fmt.Errorf("tidying modules: %w", err)
	}

	return nil
}

func processFindings[T any](oldVerFindings, newVerFindings []T, equal func(a, b T) bool) ([]T, []T, []T) {
	var (
		fixedIssues []T
		staleIssues []T
		newIssues   []T
	)

	for _, issue := range oldVerFindings {
		idx := slices.IndexFunc(newVerFindings, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			fixedIssues = append(fixedIssues, issue)
		} else {
			staleIssues = append(staleIssues, newVerFindings[idx])
		}
	}
	for _, issue := range newVerFindings {
		idx := slices.IndexFunc(oldVerFindings, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			newIssues = append(newIssues, issue)
		}
	}

	return fixedIssues, staleIssues, newIssues
}

func printLinterIssues(issues []lintIssue) {
	for _, issue := range issues {
		srcLines := strings.Join(issue.SourceLines, "\n")

		fmt.Printf("(%s) %s: %s:%d:%d:\n%s\n\n", issue.FromLinter, issue.Text, issue.Pos.Filename, issue.Pos.Line, issue.Pos.Column, srcLines)
	}
}

func printCaps(caps []capability) {
	for _, cap := range caps {
		fmt.Printf("%s: %s\n", cap.Capability, cap.CapabilityType)
		for i, call := range cap.Path {
			if i == 0 {
				fmt.Println(call.Name)
				continue
			}

			if call.Site.Filename != "" {
				fmt.Printf("  %s %s:%s:%s\n",
					call.Name,
					call.Site.Filename,
					call.Site.Line,
					call.Site.Column,
				)
				continue
			}
			fmt.Printf("  %s\n", call.Name)
		}

		fmt.Print("\n\n")
	}
}

func trimFilename(path, goModCache string) string {
	return strings.TrimPrefix(path, goModCache+string(filepath.Separator))
}
