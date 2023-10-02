package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/pkg/browser"
	"github.com/samber/lo"
	"golang.org/x/mod/modfile"
)

const (
	projectName = "Dep Inspector"

	tempPrefix = "dep-inspector"
)

var goEnvVars = []string{
	"HOME",
	"PATH",
}

func usage() {
	fmt.Fprintf(os.Stderr, `
<Project description>

	dep-inspector [flags]

<Project details/usage>

%s accepts the following flags:

`[1:], projectName)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `

For more information, see https://github.com/capnspacehook/dep-inspector.
`[1:])
}

func main() {
	os.Exit(mainRetCode())
}

type depInspector struct {
	inspectAllPkgs bool
	htmlOutput     bool
	verbose        bool

	modFile  *modfile.File
	modCache string
}

func mainRetCode() int {
	var (
		de           depInspector
		printVersion bool
	)

	flag.Usage = usage
	flag.BoolVar(&de.inspectAllPkgs, "a", false, "inspect all packages of the dependency, not just those that are used")
	flag.BoolVar(&de.htmlOutput, "html", false, "output findings in html")
	flag.BoolVar(&de.verbose, "v", false, "print commands being run and verbose information")
	flag.BoolVar(&printVersion, "version", false, "print version and build information and exit")
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := mainErr(ctx, de); err != nil {
		var exitErr *errJustExit
		if errors.As(err, &exitErr) {
			return int(*exitErr)
		}
		log.Printf("error: %v", err)
		return 1
	}

	return 0
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

func mainErr(ctx context.Context, inspector depInspector) error {
	if err := inspector.init(ctx); err != nil {
		return err
	}

	if flag.NArg() == 1 {
		depVer := flag.Arg(0)
		dep, ver, ok := strings.Cut(depVer, "@")
		if !ok {
			// TODO: support not passing version and just using what's in go.mod
			log.Println(`malformed version string: no "@" present`)
			usage()
			return errJustExit(2)
		}

		err := inspector.inspectSingleDep(ctx, dep, ver)
		if err != nil {
			return err
		}
		return nil
	}

	dep := flag.Arg(0)
	oldVer := flag.Arg(1)
	newVer := flag.Arg(2)
	err := inspector.compareDepVersions(ctx, dep, oldVer, newVer)
	if err != nil {
		return err
	}

	return nil
}

func (d *depInspector) init(ctx context.Context) (err error) {
	d.modFile, err = d.parseGoMod(ctx)
	if err != nil {
		return err
	}
	d.modCache, err = d.getGoModCache(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (d *depInspector) inspectSingleDep(ctx context.Context, dep, version string) error {
	capResult, lintIssues, err := d.inspectDep(ctx, dep, version)
	if err != nil {
		return err
	}

	if d.htmlOutput {
		r, err := d.singleDepHTMLOutput(ctx, dep, version, capResult, lintIssues)
		if err != nil {
			return err
		}
		err = browser.OpenReader(r)
		if err != nil {
			return err
		}
		return nil
	}

	printCaps(capResult.CapabilityInfo)
	printLinterIssues(lintIssues)

	return nil
}

func (d *depInspector) inspectDep(ctx context.Context, dep, version string) (*capslockResult, []lintIssue, error) {
	versionStr := makeVersionStr(dep, version)
	if err := d.setupDepVersion(ctx, versionStr); err != nil {
		return nil, nil, fmt.Errorf("setting up dependency: %w", err)
	}

	pkgs, err := listPackages(d.modFile.Module.Mod.Path)
	if err != nil {
		return nil, nil, err
	}

	var (
		capsCh   = make(chan *capslockResult, 1)
		issuesCh = make(chan []lintIssue, 1)
		errCh    = make(chan error, 2)
		wg       sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()

		capResult, err := d.findCapabilities(ctx, dep, versionStr, pkgs)
		if err != nil {
			errCh <- fmt.Errorf("finding capabilities of dependency: %w", err)
			return
		}
		capsCh <- capResult
	}()
	go func() {
		defer wg.Done()

		issues, err := d.lintDepVersion(ctx, dep, versionStr, pkgs)
		if err != nil {
			errCh <- fmt.Errorf("linting dependency: %w", err)
			return
		}
		issuesCh <- issues
	}()

	wg.Wait()
	close(errCh)

	var inspectErrs []error
	for err := range errCh {
		inspectErrs = append(inspectErrs, err)
	}
	if len(inspectErrs) != 0 {
		return nil, nil, errors.Join(inspectErrs...)
	}

	return <-capsCh, <-issuesCh, nil
}

func (d *depInspector) compareDepVersions(ctx context.Context, dep, oldVer, newVer string) error {
	results, err := d.inspectDepVersions(ctx, dep, oldVer, newVer)
	if err != nil {
		return err
	}

	if d.htmlOutput {
		r, err := d.compareDepsHTMLOutput(ctx, dep, oldVer, newVer, results)
		if err != nil {
			return err
		}

		err = browser.OpenReader(r)
		if err != nil {
			return err
		}
		return nil
	}

	printDepComparison(results)

	return nil
}

type inspectResults struct {
	fixedIssues []lintIssue
	staleIssues []lintIssue
	newIssues   []lintIssue

	capMods     []capModule
	removedCaps []capability
	staleCaps   []capability
	addedCaps   []capability
}

func (d *depInspector) inspectDepVersions(ctx context.Context, dep, oldVer, newVer string) (*inspectResults, error) {
	// inspect old version
	oldCaps, oldLintIssues, err := d.inspectDep(ctx, dep, oldVer)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", makeVersionStr(dep, oldVer), err)
	}

	// inspect new version
	newCaps, newLintIssues, err := d.inspectDep(ctx, dep, newVer)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", makeVersionStr(dep, newVer), err)
	}

	// process linter issues and capabilities
	fixedIssues, staleIssues, newIssues := processFindings(oldLintIssues, newLintIssues, func(a, b lintIssue) bool {
		return issuesEqual(dep, a, b)
	})
	removedCaps, staleCaps, addedCaps := processFindings(oldCaps.CapabilityInfo, newCaps.CapabilityInfo, capsEqual)

	capMods := append(oldCaps.ModuleInfo, newCaps.ModuleInfo...)
	slices.SortFunc(capMods, func(a, b capModule) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		return 0
	})
	capMods = slices.CompactFunc(capMods, func(a, b capModule) bool {
		return a.Path == b.Path
	})

	return &inspectResults{
		fixedIssues: fixedIssues,
		staleIssues: staleIssues,
		newIssues:   newIssues,
		capMods:     capMods,
		removedCaps: removedCaps,
		staleCaps:   staleCaps,
		addedCaps:   addedCaps,
	}, nil
}

func (d *depInspector) parseGoMod(ctx context.Context) (*modfile.File, error) {
	var output bytes.Buffer
	err := d.runCommand(ctx, &output, "go", "env", "GOMOD")
	if err != nil {
		return nil, fmt.Errorf("finding GOMOD: %w", err)
	}

	modFilePath := trimNewline(output.String())
	modFileContents, err := os.ReadFile(modFilePath)
	if err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	modFile, err := modfile.Parse(modFilePath, modFileContents, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing go.mod: %w", err)
	}

	return modFile, err
}

func trimNewline(s string) string {
	if len(s) != 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}

func (d *depInspector) getGoModCache(ctx context.Context) (string, error) {
	var sb strings.Builder
	err := d.runCommand(ctx, &sb, "go", "env", "GOMODCACHE")
	if err != nil {
		return "", fmt.Errorf("getting GOMODCACHE: %w", err)
	}
	// 'go env' output always ends with a newline
	if sb.Len() < 2 {
		return "", errors.New("GOMODCACHE is empty")
	}

	return sb.String()[:sb.Len()-1], nil
}

func (d *depInspector) setupDepVersion(ctx context.Context, versionStr string) error {
	// add dep to go.mod so linting it will work
	err := d.runGoCommand(ctx, "go", "get", versionStr)
	if err != nil {
		return fmt.Errorf("downloading %q: %w", versionStr, err)
	}
	err = d.runGoCommand(ctx, "go", "mod", "tidy")
	if err != nil {
		return fmt.Errorf("tidying modules: %w", err)
	}

	return nil
}

func processFindings[T any](oldVerFindings, newVerFindings []T, equal func(a, b T) bool) ([]T, []T, []T) {
	var (
		removedFindings []T
		staleFindings   []T
		newFindings     []T
	)

	for _, issue := range oldVerFindings {
		idx := slices.IndexFunc(newVerFindings, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			removedFindings = append(removedFindings, issue)
		} else {
			staleFindings = append(staleFindings, newVerFindings[idx])
		}
	}
	for _, issue := range newVerFindings {
		idx := slices.IndexFunc(oldVerFindings, func(issue2 T) bool {
			return equal(issue, issue2)
		})
		if idx == -1 {
			newFindings = append(newFindings, issue)
		}
	}

	return removedFindings, staleFindings, newFindings
}

type findingTotals struct {
	TotalCaps   int
	DirectCaps  int
	Caps        map[string]int
	TotalIssues int
	Issues      map[string]int
}

func calculateTotals(caps []capability, issues []lintIssue) findingTotals {
	t := findingTotals{
		TotalCaps:   len(caps),
		TotalIssues: len(issues),
	}

	t.DirectCaps = lo.CountBy(caps, func(cap capability) bool {
		return cap.CapabilityType == "CAPABILITY_TYPE_DIRECT"
	})
	t.Caps = lo.CountValuesBy(caps, func(cap capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(cap.Capability, "CAPABILITY_"), "_", " ")
		return strings.Title(strings.ToLower(capName))
	})
	t.Issues = lo.CountValuesBy(issues, func(issue lintIssue) string {
		if strings.HasPrefix(issue.FromLinter, "staticcheck") {
			return "staticcheck"
		}
		return issue.FromLinter
	})
	return t
}

func makeVersionStr(dep, version string) string {
	return dep + "@" + version
}
