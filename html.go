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
)

//go:embed output/single-html.tmpl
var htmlTmpl string

type result struct {
	Dep        string
	VersionStr string
	RemoteURL  *url.URL

	LinterIssues []lintIssue
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

	funcMap := map[string]any{
		"posToURL": func(pos token.Position) string {
			// make the tag not Go specific
			ver := strings.TrimSuffix(version, "+incompatible")
			return posToURL(pos, ver, remoteURL)
		},
	}

	tmpl, err := template.New("output").Funcs(funcMap).Parse(htmlTmpl)
	if err != nil {
		return nil, fmt.Errorf("error parsing output template: %w", err)
	}

	res := &result{
		Dep:          dep,
		VersionStr:   makeVersionStr(dep, version),
		RemoteURL:    remoteURL,
		LinterIssues: issues,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, res); err != nil {
		return nil, fmt.Errorf("error executing output template: %w", err)
	}

	return &buf, nil
}

func posToURL(pos token.Position, version string, remoteURL *url.URL) string {
	// TODO: handle other remotes than Github

	newURL := *remoteURL
	newURL.Fragment = "L" + strconv.Itoa(pos.Line)
	newURL.Path = path.Join(newURL.Path, "blob", version, pos.Filename)

	return newURL.String()
}
