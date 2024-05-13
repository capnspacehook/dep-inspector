package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/pkg/browser"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const (
	projectName = "Dep Inspector"

	curVersion = "current"
	tempPrefix = "dep-inspector"
)

var goEnvVars = []string{
	"HOME",
	"PATH",
}

func usage() {
	fmt.Fprintf(os.Stderr, `
dep-inspector allows you to find used capabilities and potential
correctness issues in a dependency version or compare between 
dependency versions.

To inspect a single dependency version:

	dep-inspector [flags] path/of/module@version

To compare dependency versions:

	dep-inspector [flags] path/of/module old-version new-version

'current' can be used instead of a version if you wish to inspect or
compare the current version of a dependency.

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
	unusedDep      bool
	outputFile     string
	verbose        bool

	parsedModFile *modfile.File
	modCache      string

	modFile       *os.File
	sumFile       *os.File
	modBackupFile *os.File
	sumBackupFile *os.File
}

func mainRetCode() int {
	var (
		de           depInspector
		printVersion bool
	)

	flag.Usage = usage
	flag.BoolVar(&de.inspectAllPkgs, "a", false, "inspect all packages of the dependency, not just those that are used")
	flag.BoolVar(&de.unusedDep, "unused-dep", false, "inspect dependency that is not used in this module")
	flag.StringVar(&de.outputFile, "o", "", "file to write output HTML to")
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

	if err := mainErr(ctx, &de); err != nil {
		var exitErr errJustExit
		if errors.As(err, &exitErr) {
			return int(exitErr)
		}
		log.Printf("error: %v", err)
		return 1
	}

	return 0
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

func mainErr(ctx context.Context, de *depInspector) (ret error) {
	if err := de.init(ctx); err != nil {
		return err
	}
	defer func() {
		restoreErr := de.restoreGoMod()
		if ret != nil {
			ret = errors.Join(ret, restoreErr)
		} else {
			ret = restoreErr
		}
	}()

	if flag.NArg() == 1 {
		depVer := flag.Arg(0)
		dep, ver, ok := strings.Cut(depVer, "@")
		if !ok {
			// TODO: support not passing version and just using what's in go.mod
			log.Println(`malformed module version string: no "@" present`)
			usage()
			return errJustExit(2)
		}
		ver, err := de.checkVersion(dep, ver)
		if err != nil {
			return err
		}

		return de.inspectSingleDep(ctx, dep, ver)
	}

	dep := flag.Arg(0)
	oldVer, err := de.checkVersion(dep, flag.Arg(1))
	if err != nil {
		return fmt.Errorf("checking old version: %w", err)
	}
	newVer, err := de.checkVersion(dep, flag.Arg(2))
	if err != nil {
		return fmt.Errorf("checking new version: %w", err)
	}
	if oldVer == newVer {
		return errors.New("cannot compare; old version and new version are the same")
	}

	return de.compareDepVersions(ctx, dep, oldVer, newVer)
}

func (d *depInspector) init(ctx context.Context) (err error) {
	if err := d.parseAndBackupGoMod(ctx); err != nil {
		return err
	}
	d.modCache, err = d.getGoModCache(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (d *depInspector) checkVersion(dep, ver string) (string, error) {
	if ver == "" {
		return "", errors.New("version is empty")
	}

	if ver == curVersion {
		if d.unusedDep {
			return "", errors.New("finding the current version and -unused-dep are mutually exclusive")
		}

		for _, requiredDep := range d.parsedModFile.Require {
			if requiredDep.Mod.Path == dep {
				ver = requiredDep.Mod.Version
			}
		}
		if ver == curVersion {
			return "", fmt.Errorf("an entry in go.mod could not be found for %q", dep)
		}
	}

	if ver[0] != 'v' {
		ver = "v" + ver
	}
	if !semver.IsValid(ver) {
		return "", fmt.Errorf("%q is not a valid module version", ver)
	}

	return ver, nil
}

func (d *depInspector) inspectSingleDep(ctx context.Context, dep, version string) error {
	capResult, lintIssues, err := d.inspectDep(ctx, dep, version)
	if err != nil {
		return err
	}

	r, err := d.singleDepHTMLOutput(ctx, dep, version, capResult, lintIssues)
	if err != nil {
		return err
	}

	if d.outputFile != "" {
		outFile, err := os.Create(d.outputFile)
		if err != nil {
			return err
		}
		defer outFile.Close()
		_, err = io.Copy(outFile, r)
		return err
	}

	err = browser.OpenReader(r)
	if err != nil {
		return err
	}

	return nil
}

func (d *depInspector) inspectDep(ctx context.Context, dep, version string) (*capslockResult, []*lintIssue, error) {
	versionStr := makeVersionStr(dep, version)
	if err := d.setupDepVersion(ctx, versionStr); err != nil {
		return nil, nil, fmt.Errorf("setting up dependency: %w", err)
	}

	modPath := d.parsedModFile.Module.Mod.Path
	pkgs, err := listPackages(modPath)
	if err != nil {
		return nil, nil, err
	}
	// if -unused-dep wasn't passed make sure the dependency is actually
	// dependency or running tools will fail
	if !d.unusedDep {
		var depIsUsed bool
		for _, pkg := range pkgs {
			if pkg.Module != nil && pkg.Module.Path == dep {
				depIsUsed = true
				break
			}
		}
		if !depIsUsed {
			return nil, nil, fmt.Errorf("%s is not used in %s, run again with the -unused-dep flag", versionStr, modPath)
		}
	}

	var (
		capsCh   = make(chan *capslockResult, 1)
		issuesCh = make(chan []*lintIssue, 1)
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

		issues, err := d.lintDepVersion(ctx, dep, version, pkgs)
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

	r, err := d.compareDepsHTMLOutput(ctx, dep, oldVer, newVer, results)
	if err != nil {
		return err
	}

	if d.outputFile != "" {
		outFile, err := os.Create(d.outputFile)
		if err != nil {
			return err
		}
		defer outFile.Close()
		_, err = io.Copy(outFile, r)
		return err
	}

	err = browser.OpenReader(r)
	if err != nil {
		return err
	}
	return nil
}

type inspectResults struct {
	oldCapMods  []capModule
	newCapMods  []capModule
	removedCaps []*capability
	sameCaps    []*capability
	addedCaps   []*capability

	fixedIssues []*lintIssue
	staleIssues []*lintIssue
	newIssues   []*lintIssue
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
	removedCaps, staleCaps, addedCaps := processFindings(oldCaps.CapabilityInfo, newCaps.CapabilityInfo, capsEqual)
	fixedIssues, staleIssues, newIssues := processFindings(oldLintIssues, newLintIssues, func(a, b *lintIssue) bool {
		return issuesEqual(dep, a, b)
	})

	return &inspectResults{
		oldCapMods:  oldCaps.ModuleInfo,
		newCapMods:  newCaps.ModuleInfo,
		removedCaps: removedCaps,
		sameCaps:    staleCaps,
		addedCaps:   addedCaps,
		fixedIssues: fixedIssues,
		staleIssues: staleIssues,
		newIssues:   newIssues,
	}, nil
}

func (d *depInspector) parseAndBackupGoMod(ctx context.Context) (ret error) {
	// parse go.mod
	var output bytes.Buffer
	err := d.runCommand(ctx, &output, "go", "env", "GOMOD")
	if err != nil {
		return fmt.Errorf("finding GOMOD: %w", err)
	}
	modFilePath := trimNewline(output.String())

	// ensure all files will be closed if an error occurred
	defer func() {
		if ret != nil {
			ret = errors.Join(ret, d.closeFiles())
		}
	}()

	d.modFile, err = os.OpenFile(modFilePath, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	output.Reset()
	if _, err := io.Copy(&output, d.modFile); err != nil {
		return fmt.Errorf("reading go.mod: %w", err)
	}
	d.parsedModFile, err = modfile.Parse(modFilePath, output.Bytes(), nil)
	if err != nil {
		return fmt.Errorf("parsing go.mod: %w", err)
	}

	// create backups of go.mod and go.sum so we can restore them after
	// analysis is finished
	d.modBackupFile, err = os.CreateTemp("", "go.mod.bak")
	if err != nil {
		return fmt.Errorf("creating backup go.mod file: %w", err)
	}
	d.sumBackupFile, err = os.CreateTemp("", "go.sum.bak")
	if err != nil {
		return fmt.Errorf("creating backup go.sum file: %w", err)
	}

	if _, err := io.Copy(d.modBackupFile, &output); err != nil {
		return fmt.Errorf("copying go.mod: %w", err)
	}
	if err := d.modBackupFile.Sync(); err != nil {
		return err
	}
	sumFilePath := filepath.Join(filepath.Dir(modFilePath), "go.sum")
	d.sumFile, err = os.OpenFile(sumFilePath, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(d.sumBackupFile, d.sumFile); err != nil {
		return fmt.Errorf("copying go.sum: %w", err)
	}
	if err := d.sumBackupFile.Sync(); err != nil {
		return err
	}

	return err
}

func (d *depInspector) restoreGoMod() (ret error) {
	// ensure all files will be closed and errors reported
	defer func() {
		closeErr := d.closeFiles()
		if ret != nil {
			ret = errors.Join(ret, closeErr)
		} else {
			ret = closeErr
		}
	}()

	// truncate go.mod and go.sum in case they are currently larger
	// than the size of the backups
	if err := d.modFile.Truncate(0); err != nil {
		return fmt.Errorf("truncating go.mod: %w", err)
	}
	if err := d.sumFile.Truncate(0); err != nil {
		return fmt.Errorf("truncating go.sum: %w", err)
	}

	seekers := []io.Seeker{
		d.modFile,
		d.sumFile,
		d.modBackupFile,
		d.sumBackupFile,
	}
	for _, seeker := range seekers {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}

	if _, err := io.Copy(d.modFile, d.modBackupFile); err != nil {
		return fmt.Errorf("restoring go.mod: %w", err)
	}
	if _, err := io.Copy(d.sumFile, d.sumBackupFile); err != nil {
		return fmt.Errorf("restoring go.sum: %w", err)
	}

	return nil
}

func (d *depInspector) closeFiles() error {
	closers := []io.Closer{
		d.modFile,
		d.sumFile,
		d.modBackupFile,
		d.sumBackupFile,
	}
	var errs []error
	for _, closer := range closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
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
	log.Printf("setting up %s", versionStr)
	// add dep to go.mod so linting it will work
	err := d.runGoCommand(ctx, "go", "get", versionStr)
	if err != nil {
		return fmt.Errorf("downloading %q: %w", versionStr, err)
	}
	if !d.unusedDep {
		err = d.runGoCommand(ctx, "go", "mod", "tidy")
		if err != nil {
			return fmt.Errorf("tidying modules: %w", err)
		}
	}

	return nil
}

func processFindings[T any](oldVerFindings, newVerFindings []T, equal func(a, b T) bool) ([]T, []T, []T) {
	var (
		allFindingsLen = len(oldVerFindings) + len(newVerFindings)

		removedFindings = make([]T, 0, allFindingsLen/4)
		staleFindings   = make([]T, 0, allFindingsLen/2)
		newFindings     = make([]T, 0, allFindingsLen/4)
	)

	for _, finding := range oldVerFindings {
		idx := slices.IndexFunc(newVerFindings, func(finding2 T) bool {
			return equal(finding, finding2)
		})
		if idx == -1 {
			removedFindings = append(removedFindings, finding)
		} else {
			staleFindings = append(staleFindings, newVerFindings[idx])
		}
	}
	for _, finding := range newVerFindings {
		idx := slices.IndexFunc(oldVerFindings, func(finding2 T) bool {
			return equal(finding, finding2)
		})
		if idx == -1 {
			newFindings = append(newFindings, finding)
		}
	}

	removedFindings = slices.Clip(removedFindings)
	staleFindings = slices.Clip(staleFindings)
	newFindings = slices.Clip(newFindings)

	return removedFindings, staleFindings, newFindings
}

func makeVersionStr(dep, version string) string {
	return dep + "@" + version
}
