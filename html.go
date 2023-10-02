package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"go/token"
	"html/template"
	"io"
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
	"golang.org/x/exp/maps"
	"golang.org/x/mod/module"
)

//go:embed output/*
var tmplFS embed.FS

type singleDepResult struct {
	Dep              string
	VersionStr       string
	ModuleRemoteURLs map[string]moduleURL

	Findings findingResult
}

type moduleURL struct {
	version     string
	verIsCommit bool
	url         *url.URL
}

type findingResult struct {
	Caps   map[string][]capability
	Issues map[string][]lintIssue
	Totals findingTotals
}

func (d *depInspector) singleDepHTMLOutput(ctx context.Context, dep, version string, capResult *capslockResult, issues []lintIssue) (io.Reader, error) {
	capMods, modURLs, err := findModuleURLs(capResult.ModuleInfo)
	if err != nil {
		return nil, err
	}
	goVer, stdlibURL, err := d.findStdlibURL(ctx)
	if err != nil {
		return nil, err
	}
	tmpl, err := loadTemplate("output/single-dep.tmpl", dep, capMods, modURLs, goVer, stdlibURL)
	if err != nil {
		return nil, err
	}

	res := &singleDepResult{
		Dep:              dep,
		VersionStr:       makeVersionStr(dep, version),
		ModuleRemoteURLs: modURLs,
		Findings:         prepareFindingResult(dep, capResult.CapabilityInfo, issues),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, res); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}

	return &buf, nil
}

type compareDepsResult struct {
	Dep              string
	OldVersionStr    string
	NewVersionStr    string
	ModuleRemoteURLs map[string]moduleURL

	OldFindings  findingResult
	SameFindings findingResult
	NewFindings  findingResult
}

func (d *depInspector) compareDepsHTMLOutput(ctx context.Context, dep, oldVer, newVer string, results *inspectResults) (io.Reader, error) {
	capMods, modURLs, err := findModuleURLs(results.capMods)
	if err != nil {
		return nil, err
	}
	goVer, stdlibURL, err := d.findStdlibURL(ctx)
	if err != nil {
		return nil, err
	}
	tmpl, err := loadTemplate("output/compare-deps.tmpl", dep, capMods, modURLs, goVer, stdlibURL)
	if err != nil {
		return nil, err
	}

	res := &compareDepsResult{
		Dep:              dep,
		OldVersionStr:    makeVersionStr(dep, oldVer),
		NewVersionStr:    makeVersionStr(dep, newVer),
		ModuleRemoteURLs: modURLs,
		OldFindings:      prepareFindingResult(dep, results.removedCaps, results.fixedIssues),
		SameFindings:     prepareFindingResult(dep, results.staleCaps, results.staleIssues),
		NewFindings:      prepareFindingResult(dep, results.addedCaps, results.newIssues),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, res); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}

	return &buf, nil
}

func loadTemplate(tmplPath, dep string, capMods []string, modURLs map[string]moduleURL, goVer string, stdlibURL *url.URL) (*template.Template, error) {
	funcMap := map[string]any{
		"getCapsByPkg": func(caps []capability) map[string][]capability {
			return lo.GroupBy(caps, func(cap capability) string {
				return cap.PackageDir
			})
		},
		"getCapsByFinalCall": func(caps []capability) map[string][]capability {
			return lo.GroupBy(caps, func(cap capability) string {
				return cap.Path[len(cap.Path)-1].Name
			})
		},
		"capType": func(capType string) string {
			if capType == "CAPABILITY_TYPE_DIRECT" {
				return "Direct"
			}
			return "Transitive"
		},
		"getIssuesByLinter": func(issues []lintIssue) map[string][]lintIssue {
			return lo.GroupBy(issues, func(issue lintIssue) string {
				return issue.FromLinter
			})
		},
		"getPrevCallName": func(calls []functionCall, idx int) string {
			return calls[idx-1].Name
		},
		"capPosToURL": func(call functionCall, prevCallName string) (string, error) {
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
				return callSiteToURL(call.Site, modURL, pkg), nil
			}

			modURL = modURLs[capMods[i]]
			pkgAndCall := strings.TrimPrefix(name, capMods[i])
			lastSlashIdx := strings.LastIndex(pkgAndCall, "/")
			if lastSlashIdx == -1 {
				pkg, _, ok := strings.Cut(pkgAndCall, ".")
				if !ok {
					return "", fmt.Errorf("malformed capability call site %q", call.Name)
				}
				return callSiteToURL(call.Site, modURL, pkg), nil
			}

			pkg, _, ok := strings.Cut(pkgAndCall[lastSlashIdx:], ".")
			if !ok {
				return "", fmt.Errorf("malformed capability call site %q", call.Name)
			}
			pkg = path.Join(pkgAndCall[:lastSlashIdx], pkg)

			return callSiteToURL(call.Site, modURL, pkg), nil
		},
		"issuePosToURL": func(pos token.Position) string {
			site := callSite{
				Filename: pos.Filename,
				Line:     strconv.Itoa(pos.Line),
			}
			// no need to pass the package here, the filenames already
			// have the package prefixed
			return callSiteToURL(site, modURLs[dep], "")
		},
	}

	tmpl, err := template.ParseFS(tmplFS, tmplPath)
	if err != nil {
		return nil, fmt.Errorf("error parsing output template: %w", err)
	}
	tmpl = tmpl.Funcs(funcMap)
	tmpl, err = tmpl.ParseFS(tmplFS, "output/capabilities.tmpl", "output/linter-issues.tmpl", "output/totals.tmpl")
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

func findModuleURL(mod, version, localPath string) (moduleURL, error) {
	remote := "https://" + mod
	if strings.HasPrefix(mod, "golang.org/x/") {
		remote = "https://github.com/golang/" + strings.TrimPrefix(mod, "golang.org/x/")
	}
	if !strings.HasPrefix(mod, "github.com/") && !strings.HasPrefix(mod, "gitlab.com/") {
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

func prepareFindingResult(dep string, caps []capability, issues []lintIssue) (f findingResult) {
	f.Caps = lo.GroupBy(caps, func(cap capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(cap.Capability, "CAPABILITY_"), "_", " ")
		return strings.Title(strings.ToLower(capName))
	})
	f.Issues = lo.GroupBy(issues, func(issue lintIssue) string {
		return path.Join(dep, path.Dir(issue.Pos.Filename))
	})
	f.Totals = calculateTotals(caps, issues)

	return f
}

func callSiteToURL(site callSite, modURL moduleURL, pkg string) string {
	if site.Filename == "" {
		return ""
	}

	newURL := *modURL.url
	newURL.Fragment = "L" + site.Line
	filename := path.Join(pkg, site.Filename)

	switch newURL.Host {
	case "github.com":
		newURL.Path = path.Join(newURL.Path, "blob", modURL.version, filename)
	case "gitlab.com":
		newURL.Path = path.Join(newURL.Path, "-", "blob", modURL.version, filename)
	case "gittea.dev":
		srcType := "tag"
		if modURL.verIsCommit {
			srcType = "commit"
		}
		newURL.Path = path.Join(newURL.Path, "src", srcType, modURL.version, filename)
	default:
		return filename + ":" + site.Line
	}

	return newURL.String()
}
