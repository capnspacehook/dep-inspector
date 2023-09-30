package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/token"
	"html/template"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/Masterminds/vcs"
	"github.com/samber/lo"
	"golang.org/x/mod/module"
)

//go:embed output/single-html.tmpl
var htmlTmpl string

type result struct {
	Dep        string
	VersionStr string
	RemoteURL  *url.URL

	IssuePkgs map[string][]lintIssue
}

func formatHTMLOutput(dep, version string, caps []capability, issues []lintIssue) (io.Reader, error) {
	local, err := os.MkdirTemp("", tempPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(local)

	remote := "https://" + dep
	if strings.HasPrefix(dep, "golang.org/x/") {
		remote = "https://github.com/golang/" + strings.TrimPrefix(dep, "golang.org/x/")
	}
	if !strings.HasPrefix(dep, "github.com/") {
		repo, err := vcs.NewRepo(remote, local)
		if err != nil {
			return nil, fmt.Errorf("error finding remote repository for dependency: %w", err)
		}
		remote = repo.Remote()
	}
	remoteURL, err := url.Parse(remote)
	if err != nil {
		return nil, fmt.Errorf("parsing remote URL: %w", err)
	}

	// make the version not Go specific
	ver := version
	var verIsCommit bool
	if module.IsPseudoVersion(version) {
		ver, err = module.PseudoVersionRev(version)
		if err != nil {
			return nil, fmt.Errorf("parsing module version: %w", err)
		}
		verIsCommit = true
	} else {
		ver = strings.TrimSuffix(version, "+incompatible")
	}

	funcMap := map[string]any{
		"issuesByLinter": func(issues []lintIssue) map[string][]lintIssue {
			return lo.GroupBy(issues, func(issue lintIssue) string {
				return issue.FromLinter
			})
		},
		"posToURL": func(pos token.Position) string {
			return posToURL(pos, ver, verIsCommit, remoteURL)
		},
	}

	tmpl, err := template.New("output").Funcs(funcMap).Parse(htmlTmpl)
	if err != nil {
		return nil, fmt.Errorf("error parsing output template: %w", err)
	}

	pkgIssues := lo.GroupBy(issues, func(issue lintIssue) string {
		return path.Dir(issue.Pos.Filename)
	})

	res := &result{
		Dep:        dep,
		VersionStr: makeVersionStr(dep, version),
		RemoteURL:  remoteURL,
		IssuePkgs:  pkgIssues,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, res); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}

	return &buf, nil
}

func posToURL(pos token.Position, version string, isCommit bool, remoteURL *url.URL) string {
	newURL := *remoteURL
	line := strconv.Itoa(pos.Line)
	newURL.Fragment = "L" + line

	switch newURL.Host {
	case "github.com":
		newURL.Path = path.Join(newURL.Path, "blob", version, pos.Filename)
	case "gitlab.com":
		newURL.Path = path.Join(newURL.Path, "-", "blob", version, pos.Filename)
	case "gittea.dev":
		srcType := "tag"
		if isCommit {
			srcType = "commit"
		}
		newURL.Path = path.Join(newURL.Path, "src", srcType, version, pos.Filename)
	default:
		return pos.Filename + ":" + line
	}

	return newURL.String()
}
