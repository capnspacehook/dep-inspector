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
	inspectAllPkgs   bool
	unusedDep        bool
	upgradeTransDeps bool
	outputFile       string
	verbose          bool

	modFilePath   string
	sumFilePath   string
	parsedModFile *modfile.File
	modCache      string

	modBackupFiles    *modFilePair
	oldModBackupFiles *modFilePair
	newModBackupFiles *modFilePair
}

type modFilePair struct {
	modFile *os.File
	sumFile *os.File
}

func (m *modFilePair) Close() error {
	return errors.Join(m.modFile.Close(), m.sumFile.Close())
}

func mainRetCode() int {
	var (
		de           depInspector
		printVersion bool
	)

	flag.Usage = usage
	flag.BoolVar(&de.inspectAllPkgs, "a", false, "inspect all packages of the dependency, not just those that are used")
	flag.BoolVar(&de.unusedDep, "unused-dep", false, "inspect dependency that is not used in this module")
	flag.BoolVar(&de.upgradeTransDeps, "u", false, "upgrade transitive dependencies and inspect them as well")
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
		restoreErr := de.restoreGoMod(de.modBackupFiles)
		closeErr := de.closeFiles()
		ret = errors.Join(ret, restoreErr, closeErr)
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

		return de.inspectSingleDepVersion(ctx, dep, ver)
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
		return errors.New("cannot compare: old version and new version are the same")
	}
	if semver.Compare(oldVer, newVer) == 1 {
		return fmt.Errorf("cannot compare: %q is greater than %q. old version must be less than new version", oldVer, newVer)
	}

	return de.compareDepVersionsRecursively(ctx, dep, oldVer, newVer)
}

func (d *depInspector) init(ctx context.Context) error {
	d.modBackupFiles = new(modFilePair)
	d.oldModBackupFiles = new(modFilePair)
	d.newModBackupFiles = new(modFilePair)

	// open go.mod and go.sum
	var output bytes.Buffer
	err := d.runCommand(ctx, &output, "go", "env", "GOMOD")
	if err != nil {
		return fmt.Errorf("finding GOMOD: %w", err)
	}
	d.modFilePath = trimNewline(output.String())
	d.sumFilePath = filepath.Join(filepath.Dir(d.modFilePath), "go.sum")

	d.parsedModFile, err = d.parseAndBackupGoMod(d.modBackupFiles)
	if err != nil {
		return err
	}
	d.modCache, err = d.getGoModCache(ctx)
	if err != nil {
		return err
	}

	return nil
}

func (d *depInspector) openModFiles() (*modFilePair, error) {
	var (
		files = new(modFilePair)
		err   error
	)

	files.modFile, err = os.OpenFile(d.modFilePath, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	files.sumFile, err = os.OpenFile(d.sumFilePath, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	return files, nil
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

func (d *depInspector) inspectSingleDepVersion(ctx context.Context, dep, version string) error {
	capResult, lintIssues, err := d.inspectDep(ctx, d.newModBackupFiles, dep, version, true)
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

func (d *depInspector) inspectDep(ctx context.Context, modBackupFiles *modFilePair, dep, version string, newDepVer bool) (*capslockResult, []*lintIssue, error) {
	versionStr := makeVersionStr(dep, version)
	if err := d.setupDepVersion(ctx, modBackupFiles, versionStr, newDepVer); err != nil {
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

type changedDep struct {
	dep    string
	oldVer string
	newVer string
}

func (d *depInspector) compareDepVersionsRecursively(ctx context.Context, dep, oldVer, newVer string) error {
	if err := d.setupDepVersion(ctx, d.oldModBackupFiles, makeVersionStr(dep, oldVer), false); err != nil {
		return fmt.Errorf("setting up dependency: %w", err)
	}
	oldModFile, err := d.parseAndBackupGoMod(d.oldModBackupFiles)
	if err != nil {
		return err
	}
	if err := d.setupDepVersion(ctx, d.newModBackupFiles, makeVersionStr(dep, newVer), true); err != nil {
		return fmt.Errorf("setting up dependency: %w", err)
	}
	newModFile, err := d.parseAndBackupGoMod(d.newModBackupFiles)
	if err != nil {
		return err
	}

	var depsToInspect []changedDep
	for _, newDep := range newModFile.Require {
		var found bool
		for _, oldDep := range oldModFile.Require {
			if oldDep.Mod.Path != newDep.Mod.Path {
				continue
			}

			modPath := oldDep.Mod.Path
			if modPath == dep && oldDep.Mod.Version != oldVer {
				return fmt.Errorf("cannot compare: after getting %s@%s and tidying the module version is %s", dep, oldVer, oldDep.Mod.Version)
			}
			if modPath == dep && newDep.Mod.Version != newVer {
				return fmt.Errorf("cannot compare: after getting %s@%s and tidying the module version is %s", dep, newVer, newDep.Mod.Version)
			}

			found = true
			if oldDep.Mod.Version != newDep.Mod.Version {
				depsToInspect = append(depsToInspect, changedDep{
					dep:    oldDep.Mod.Path,
					oldVer: oldDep.Mod.Version,
					newVer: newDep.Mod.Version,
				})
				break
			}
		}

		if !found {
			depsToInspect = append(depsToInspect, changedDep{
				dep:    newDep.Mod.Path,
				newVer: newDep.Mod.Version,
			})
		}
	}

	for _, depToInspect := range depsToInspect {
		log.Printf("inspecting %s", depToInspect.dep)
		if depToInspect.oldVer == "" {
			err := d.inspectSingleDepVersion(ctx, depToInspect.dep, depToInspect.newVer)
			if err != nil {
				log.Printf("error inspecting newly added dep: %v", err)
			}
		} else {
			err := d.compareDepVersions(ctx, depToInspect.dep, depToInspect.oldVer, depToInspect.newVer)
			if err != nil {
				log.Printf("error comparing versions of dep: %v", err)
			}
		}
	}

	return nil
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

	return browser.OpenReader(r)
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
	oldCaps, oldLintIssues, err := d.inspectDep(ctx, d.oldModBackupFiles, dep, oldVer, false)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", makeVersionStr(dep, oldVer), err)
	}

	// inspect new version
	newCaps, newLintIssues, err := d.inspectDep(ctx, d.newModBackupFiles, dep, newVer, true)
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

func (d *depInspector) parseAndBackupGoMod(modBackupFiles *modFilePair) (_ *modfile.File, ret error) {
	modFiles, err := d.openModFiles()
	if err != nil {
		return nil, err
	}
	defer modFiles.Close()

	var output bytes.Buffer
	if _, err := io.Copy(&output, modFiles.modFile); err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	parsedModFile, err := modfile.Parse(d.modFilePath, output.Bytes(), nil)
	if err != nil {
		return nil, fmt.Errorf("parsing go.mod: %w", err)
	}

	// create backups of go.mod and go.sum so we can restore them after
	// analysis is finished
	modBackupFiles.modFile, err = os.CreateTemp("", "go.mod.bak")
	if err != nil {
		return nil, fmt.Errorf("creating backup go.mod file: %w", err)
	}
	modBackupFiles.sumFile, err = os.CreateTemp("", "go.sum.bak")
	if err != nil {
		return nil, fmt.Errorf("creating backup go.sum file: %w", err)
	}

	if _, err := io.Copy(modBackupFiles.modFile, &output); err != nil {
		return nil, fmt.Errorf("copying go.mod: %w", err)
	}
	if err := modBackupFiles.modFile.Sync(); err != nil {
		return nil, err
	}

	if _, err := io.Copy(modBackupFiles.sumFile, modFiles.sumFile); err != nil {
		return nil, fmt.Errorf("copying go.sum: %w", err)
	}
	if err := modBackupFiles.sumFile.Sync(); err != nil {
		return nil, err
	}

	return parsedModFile, err
}

func (d *depInspector) restoreGoMod(modBackupFiles *modFilePair) (ret error) {
	modFiles, err := d.openModFiles()
	if err != nil {
		return err
	}
	defer modFiles.Close()

	if err := modFiles.modFile.Truncate(0); err != nil {
		return err
	}
	if err := modFiles.sumFile.Truncate(0); err != nil {
		return err
	}

	if _, err := modBackupFiles.modFile.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := modBackupFiles.sumFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if _, err := io.Copy(modFiles.modFile, modBackupFiles.modFile); err != nil {
		return fmt.Errorf("restoring go.mod: %w", err)
	}
	if _, err := io.Copy(modFiles.sumFile, modBackupFiles.sumFile); err != nil {
		return fmt.Errorf("restoring go.sum: %w", err)
	}

	return nil
}

func (d *depInspector) closeFiles() error {
	pairs := []*modFilePair{
		d.modBackupFiles,
		d.oldModBackupFiles,
		d.newModBackupFiles,
	}
	var errs []error
	for _, filePair := range pairs {
		if filePair.modFile != nil {
			if err := filePair.modFile.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if filePair.sumFile != nil {
			if err := filePair.sumFile.Close(); err != nil {
				errs = append(errs, err)
			}
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

func (d *depInspector) setupDepVersion(ctx context.Context, modBackupFiles *modFilePair, versionStr string, newDepVersion bool) error {
	if modBackupFiles.modFile != nil && modBackupFiles.sumFile != nil {
		return d.restoreGoMod(modBackupFiles)
	}

	log.Printf("setting up %s", versionStr)
	cmd := []string{"go", "get"}
	if newDepVersion && d.upgradeTransDeps {
		cmd = append(cmd, "-u")
	}
	cmd = append(cmd, versionStr)

	// add dep to go.mod so running tools against it will work
	if err := d.runGoCommand(ctx, cmd...); err != nil {
		return fmt.Errorf("downloading %q: %w", versionStr, err)
	}
	if !d.unusedDep {
		if err := d.runGoCommand(ctx, "go", "mod", "tidy"); err != nil {
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
