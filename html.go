package main

import (
	"bytes"
	"context"
	_ "embed"
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

//go:embed output/single-html.tmpl
var htmlTmpl string

type result struct {
	Dep              string
	VersionStr       string
	ModuleRemoteURLs map[string]moduleURL

	Caps      map[string][]capability
	IssuePkgs map[string][]lintIssue
}

type moduleURL struct {
	version     string
	verIsCommit bool
	url         *url.URL
}

func (d *depInspector) formatHTMLOutput(ctx context.Context, dep, version string, capResult *capslockResult, issues []lintIssue) (io.Reader, error) {
	local, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(local)

	modURLs := make(map[string]moduleURL, len(capResult.ModuleInfo))
	for _, modInfo := range capResult.ModuleInfo {
		localPath := filepath.Join(local, strings.ReplaceAll(modInfo.Path, "/", "-"))
		if err := os.Mkdir(localPath, 0o755); err != nil {
			return nil, fmt.Errorf("creating directory: %w", err)
		}
		modURL, err := findModuleURL(modInfo.Path, modInfo.Version, localPath)
		if err != nil {
			return nil, err
		}
		modURLs[modInfo.Path] = modURL
	}
	capMods := maps.Keys(modURLs)

	// create URL for stdlib
	var verBuf bytes.Buffer
	err = d.runCommand(ctx, &verBuf, "go", "version")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`^go version (go\S+|devel \S+)`)
	m := re.FindStringSubmatch(verBuf.String())
	if len(m) != 2 {
		return nil, fmt.Errorf("unknown Go version %q", verBuf.String())
	}
	goVer := m[1]
	stdlibURL, err := url.Parse("https://github.com/golang/go")
	if err != nil {
		panic(err)
	}

	funcMap := map[string]any{
		"capsByPkg": func(caps []capability) map[string][]capability {
			return lo.GroupBy(caps, func(cap capability) string {
				return cap.PackageDir
			})
		},
		"capsByFinalCall": func(caps []capability) map[string][]capability {
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
		"issuesByLinter": func(issues []lintIssue) map[string][]lintIssue {
			return lo.GroupBy(issues, func(issue lintIssue) string {
				return issue.FromLinter
			})
		},
		"prevCallName": func(calls []functionCall, idx int) string {
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

	tmpl, err := template.New("output").Funcs(funcMap).Parse(htmlTmpl)
	if err != nil {
		return nil, fmt.Errorf("error parsing output template: %w", err)
	}

	capNameCaps := lo.GroupBy(capResult.CapabilityInfo, func(cap capability) string {
		capName := strings.ReplaceAll(strings.TrimPrefix(cap.Capability, "CAPABILITY_"), "_", " ")
		return strings.Title(strings.ToLower(capName))
	})

	pkgIssues := lo.GroupBy(issues, func(issue lintIssue) string {
		return path.Join(dep, path.Dir(issue.Pos.Filename))
	})

	res := &result{
		Dep:              dep,
		VersionStr:       makeVersionStr(dep, version),
		ModuleRemoteURLs: modURLs,
		Caps:             capNameCaps,
		IssuePkgs:        pkgIssues,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, res); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}

	return &buf, nil
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
