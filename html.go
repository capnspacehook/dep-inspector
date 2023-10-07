package main

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"go/token"
	"html/template"
	"io"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Masterminds/vcs"
	"github.com/samber/lo"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/html"
	"golang.org/x/exp/maps"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

//go:embed output/*
var tmplFS embed.FS

var supportingTmpls = []string{
	"output/capabilities.tmpl",
	"output/linter-issues.tmpl",
	"output/style.tmpl",
	"output/totals.tmpl",
}

type singleDepResult struct {
	Dep              string
	VersionStr       string
	ModuleRemoteURLs map[string]moduleURL

	Findings findingResult
}

type moduleURL struct {
	modPath     string
	version     string
	verIsCommit bool
	url         *url.URL
}

type findingResult struct {
	Caps   map[string][]*capability
	Issues map[string][]*lintIssue
	Totals findingTotals

	CapMods []string
	ModURLs map[string]moduleURL
}

func (d *depInspector) singleDepHTMLOutput(ctx context.Context, dep, version string, capResult *capslockResult, issues []*lintIssue) (io.Reader, error) {
	capMods, modURLs, err := findModuleURLs(capResult.ModuleInfo)
	if err != nil {
		return nil, err
	}
	goVer, stdlibURL, err := d.findStdlibURL(ctx)
	if err != nil {
		return nil, err
	}
	tmpl, err := d.loadTemplate("output/single-dep.tmpl", dep, capMods, goVer, stdlibURL)
	if err != nil {
		return nil, err
	}

	res := &singleDepResult{
		Dep:              dep,
		VersionStr:       makeVersionStr(dep, version),
		ModuleRemoteURLs: modURLs,
		Findings:         prepareFindingResult(dep, capResult.CapabilityInfo, issues, capMods, modURLs),
	}

	return executeTemplate(tmpl, res)
}

type compareDepsResult struct {
	Dep           string
	OldVersionStr string
	NewVersionStr string

	OldFindings  findingResult
	SameFindings findingResult
	NewFindings  findingResult
	Totals       findingTotals
}

func (d *depInspector) compareDepsHTMLOutput(ctx context.Context, dep, oldVer, newVer string, results *inspectResults) (io.Reader, error) {
	oldCapMods, oldModURLs, err := findModuleURLs(results.oldCapMods)
	if err != nil {
		return nil, err
	}
	newCapMods, newModURLs, err := findModuleURLs(results.newCapMods)
	if err != nil {
		return nil, err
	}
	capMods := append(oldCapMods, newCapMods...)
	slices.Sort(capMods)
	capMods = slices.Compact(capMods)

	goVer, stdlibURL, err := d.findStdlibURL(ctx)
	if err != nil {
		return nil, err
	}
	tmpl, err := d.loadTemplate("output/compare-deps.tmpl", dep, capMods, goVer, stdlibURL)
	if err != nil {
		return nil, err
	}

	res := &compareDepsResult{
		Dep:           dep,
		OldVersionStr: makeVersionStr(dep, oldVer),
		NewVersionStr: makeVersionStr(dep, newVer),
		OldFindings:   prepareFindingResult(dep, results.removedCaps, results.fixedIssues, oldCapMods, oldModURLs),
		SameFindings:  prepareFindingResult(dep, results.sameCaps, results.staleIssues, newCapMods, newModURLs),
		NewFindings:   prepareFindingResult(dep, results.addedCaps, results.newIssues, newCapMods, newModURLs),
	}
	buildCombinedTotals(res)

	return executeTemplate(tmpl, res)
}

func (d *depInspector) loadTemplate(tmplPath, dep string, capMods []string, goVer string, stdlibURL *url.URL) (*template.Template, error) {
	funcMap := map[string]any{
		"getCapsByPkg": func(caps []*capability) map[string][]*capability {
			return lo.GroupBy(caps, func(c *capability) string {
				return c.PackageDir
			})
		},
		"getCapsByFinalCall": func(caps []*capability) map[string][]*capability {
			return lo.GroupBy(caps, func(c *capability) string {
				return c.Path[len(c.Path)-1].Name
			})
		},
		"capType": func(capType string) string {
			if capType == "CAPABILITY_TYPE_DIRECT" {
				return "Direct"
			}
			return "Transitive"
		},
		"getIssuesByLinter": func(issues []*lintIssue) map[string][]*lintIssue {
			return lo.GroupBy(issues, func(i *lintIssue) string {
				return i.FromLinter
			})
		},
		"getPrevCallName": func(calls []functionCall, idx int) string {
			return calls[idx-1].Name
		},
		"capPosToURL": func(call functionCall, prevCallName string, modURLs map[string]moduleURL) (string, error) {
			name := strings.NewReplacer("*", "", "(", "", ")", "").Replace(prevCallName)
			i := slices.IndexFunc(capMods, func(mod string) bool {
				return strings.HasPrefix(name, mod)
			})

			var modURL moduleURL
			// module couldn't be found, is most likely stdlib
			if i == -1 {
				modURL = moduleURL{
					version: goVer,
					url:     stdlibURL,
				}
				pkg, _, ok := strings.Cut(name, ".")
				if !ok {
					return "", fmt.Errorf("malformed function name %q", name)
				}
				pkg = path.Join("src", pkg)
				return callSiteToURL(call.Site, modURL, pkg, d.modCache)
			}

			modURL = modURLs[capMods[i]]
			pkgAndCall := strings.TrimPrefix(name, capMods[i])
			lastSlashIdx := strings.LastIndex(pkgAndCall, "/")
			if lastSlashIdx == -1 {
				pkg, _, ok := strings.Cut(pkgAndCall, ".")
				if !ok {
					return "", fmt.Errorf("malformed capability call site %q", call.Name)
				}
				return callSiteToURL(call.Site, modURL, pkg, d.modCache)
			}

			pkg, _, ok := strings.Cut(pkgAndCall[lastSlashIdx:], ".")
			if !ok {
				return "", fmt.Errorf("malformed capability call site %q", call.Name)
			}
			pkg = path.Join(pkgAndCall[:lastSlashIdx], pkg)

			return callSiteToURL(call.Site, modURL, pkg, d.modCache)
		},
		"issuePosToURL": func(pos token.Position, modURLs map[string]moduleURL) (string, error) {
			site := callSite{
				Filename: pos.Filename,
				Line:     strconv.Itoa(pos.Line),
			}
			// no need to pass the package here, the filenames already
			// have the package prefixed
			return callSiteToURL(site, modURLs[dep], "", d.modCache)
		},
		"formatDelta": func(delta int) string {
			deltaStr := strconv.Itoa(delta)
			if delta >= 0 {
				deltaStr = "+" + deltaStr
			}
			return deltaStr
		},
	}

	tmpl, err := template.ParseFS(tmplFS, tmplPath)
	if err != nil {
		return nil, fmt.Errorf("error parsing output template: %w", err)
	}
	tmpl = tmpl.Funcs(funcMap)
	tmpl, err = tmpl.ParseFS(tmplFS, supportingTmpls...)
	if err != nil {
		return nil, fmt.Errorf("error parsing and associating output templates: %w", err)
	}

	return tmpl, nil
}

func findModuleURLs(capMods []capModule) ([]string, map[string]moduleURL, error) {
	local, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(local)

	modURLs := make(map[string]moduleURL, len(capMods))
	for _, modInfo := range capMods {
		localPath := filepath.Join(local, strings.ReplaceAll(modInfo.Path, "/", "-"))
		if err := os.Mkdir(localPath, 0o755); err != nil {
			return nil, nil, fmt.Errorf("creating directory: %w", err)
		}
		modURL, err := findModuleURL(modInfo.Path, modInfo.Version, localPath)
		if err != nil {
			return nil, nil, err
		}
		modURLs[modInfo.Path] = modURL
	}

	return maps.Keys(modURLs), modURLs, nil
}

func findModuleURL(modPath, version, localPath string) (moduleURL, error) {
	remote := "https://" + modPath
	if strings.HasPrefix(modPath, "golang.org/x/") {
		remote = "https://github.com/golang/" + strings.TrimPrefix(modPath, "golang.org/x/")
	}
	if !strings.HasPrefix(modPath, "github.com/") && !strings.HasPrefix(modPath, "gitlab.com/") {
		repo, err := vcs.NewRepo(remote, localPath)
		if err != nil {
			return moduleURL{}, fmt.Errorf("error finding remote repository for dependency: %w", err)
		}
		remote = repo.Remote()
	}
	remoteURL, err := url.Parse(remote)
	if err != nil {
		return moduleURL{}, fmt.Errorf("parsing remote URL: %w", err)
	}

	// make the version not Go specific
	var verIsCommit bool
	if module.IsPseudoVersion(version) {
		version, err = module.PseudoVersionRev(version)
		if err != nil {
			return moduleURL{}, fmt.Errorf("parsing module version: %w", err)
		}
		verIsCommit = true
	} else {
		version = strings.TrimSuffix(version, "+incompatible")
	}

	return moduleURL{
		modPath:     modPath,
		version:     version,
		verIsCommit: verIsCommit,
		url:         remoteURL,
	}, nil
}

func (d *depInspector) findStdlibURL(ctx context.Context) (string, *url.URL, error) {
	var verBuf bytes.Buffer
	err := d.runCommand(ctx, &verBuf, "go", "version")
	if err != nil {
		return "", nil, err
	}
	re := regexp.MustCompile(`^go version (go\S+|devel \S+)`)
	m := re.FindStringSubmatch(verBuf.String())
	if len(m) != 2 {
		return "", nil, fmt.Errorf("unknown Go version %q", verBuf.String())
	}
	goVer := m[1]
	stdlibURL, err := url.Parse("https://github.com/golang/go")
	if err != nil {
		panic(err)
	}

	return goVer, stdlibURL, nil
}

func prepareFindingResult(dep string, caps []*capability, issues []*lintIssue, capMods []string, modURLs map[string]moduleURL) (f findingResult) {
	f.Caps = lo.GroupBy(caps, func(c *capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(c.Capability, "CAPABILITY_"), "_", " ")
		//lint:ignore SA1019 the capability name will not have Unicode
		// punctuation that causes issues for strings.ToLower so using
		// it is fine
		return strings.Title(strings.ToLower(capName))
	})
	f.Issues = lo.GroupBy(issues, func(i *lintIssue) string {
		return path.Join(dep, path.Dir(i.Pos.Filename))
	})
	f.Totals = calculateTotals(caps, issues)

	f.CapMods = capMods
	f.ModURLs = modURLs

	return f
}

func executeTemplate(tmpl *template.Template, data any) (io.Reader, error) {
	var buf bytes.Buffer
	min := minify.New()
	min.AddFunc("text/html", html.Minify)
	w := min.Writer("text/html", &buf)
	if err := tmpl.Execute(w, data); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("error minifying HTML output: %w", err)
	}

	return &buf, nil
}

var v2PlusRe = regexp.MustCompile(`^v\d+$`)

func callSiteToURL(site callSite, modURL moduleURL, pkg, goModCache string) (string, error) {
	if site.Filename == "" {
		return "", nil
	}

	newURL := *modURL.url
	newURL.Fragment = "L" + site.Line
	filename := path.Join(pkg, site.Filename)

	strippedPath, err := stripMajorVersionDir(modURL.modPath, modURL.version, newURL.Path, goModCache)
	if err != nil {
		return "", err
	}
	newURL.Path = strippedPath

	// format the URL according to the hosting provider
	switch newURL.Host {
	case "github.com":
		newURL.Path = path.Join(newURL.Path, "blob", modURL.version, filename)
	case "gitlab.com":
		newURL.Path = path.Join(newURL.Path, "-", "blob", modURL.version, filename)
	case "go.googlesource.com":
		// it seems only go.googlesource.com doesn't prefix 'L' to line
		// references
		newURL.Fragment = site.Line
		if modURL.verIsCommit {
			newURL.Path = path.Join(newURL.Path, "+", "refs", "tags", modURL.version, filename)
		} else {
			newURL.Path = path.Join(newURL.Path, "+", modURL.version, filename)
		}
	case "gittea.dev":
		srcType := "tag"
		if modURL.verIsCommit {
			srcType = "commit"
		}
		newURL.Path = path.Join(newURL.Path, "src", srcType, modURL.version, filename)
	default:
		log.Printf("unknown hosting provider %s", newURL.Host)
		return filename + ":" + site.Line, nil
	}

	return newURL.String(), nil
}

// stripMajorVersionDir removes the final /vN element of a module path
// if the module is greater than v2.0.0 and the /vN element isn't a valid
// subdirectory in the source code.
func stripMajorVersionDir(modPath, version, urlPath, goModCache string) (string, error) {
	if c := semver.Compare(version, "v2.0.0"); c == -1 {
		return urlPath, nil
	}
	rest, base := path.Split(urlPath)
	if !v2PlusRe.MatchString(base) {
		return urlPath, nil
	}
	escPath, err := module.EscapePath(modPath)
	if err != nil {
		return "", err
	}
	srcPath := filepath.Join(goModCache, escPath)
	if _, err := os.Stat(srcPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rest, nil
		}
		return "", err
	}

	return urlPath, nil
}
