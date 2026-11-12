package main

import (
	"bufio"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// siteConfigs are the docs site's Hugo configs, whose baseURLs are the
// site's own hosts: devvm.dev and next.devvm.dev.
func siteConfigs() []string {
	return []string{"../../../docs/hugo.yaml", "../../../docs/next.yaml"}
}

var (
	// A tag and its attributes, in minified HTML too, where values may be
	// unquoted, and quoted ones may hold a >.
	tagRE  = regexp.MustCompile(`(?is)<([a-z][a-z0-9-]*)\b((?:[^>"']|"[^"]*"|'[^']*')*)>`)
	attrRE = regexp.MustCompile(`(?is)([a-z][a-z0-9:-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	// What CSS loads: url(…) and @import.
	cssURLRE = regexp.MustCompile(`(?i)url\(\s*["']?([^"')\s]+)|@import\s+["']([^"']+)`)
	styleRE  = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
)

// loadAttrs are the attributes a browser fetches from as the page loads,
// for a tag. A link's href is fetched unless its rel only names a page
// (navLinkRel).
func loadAttrs(tag string) []string {
	switch tag {
	case "link":
		return []string{"href"}
	case "script", "iframe", "frame", "audio", "track", "embed", "input":
		return []string{"src"}
	case "img", "source":
		return []string{"src", "srcset"}
	case "video":
		return []string{"src", "poster"}
	case "object":
		return []string{"data"}
	case "image", "use", "feimage":
		return []string{"href", "xlink:href"}
	}
	return nil
}

// navLinkRel says whether a link's rel names a page rather than something
// the browser fetches.
func navLinkRel(rel string) bool {
	switch rel {
	case "canonical", "alternate", "author", "license", "me", "next", "prev", "help", "bookmark":
		return true
	}
	return false
}

// ownHosts reads the hosts of the site's baseURLs.
func ownHosts(t *testing.T) map[string]bool {
	t.Helper()
	hosts := map[string]bool{}
	configs := siteConfigs()
	for _, path := range configs {
		f, err := os.Open(path) //nolint:gosec // the repo's own config
		if err != nil {
			t.Fatal(err)
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			if v, ok := strings.CutPrefix(s.Text(), "baseURL:"); ok {
				u, err := url.Parse(strings.Trim(strings.TrimSpace(v), `"'`))
				if err != nil || u.Hostname() == "" {
					t.Fatalf("%s: baseURL %q isn't a URL", path, v)
				}
				hosts[u.Hostname()] = true
			}
		}
		_ = f.Close()
	}
	if len(hosts) != len(configs) {
		t.Fatalf("want a baseURL in each of %v, got %v", configs, hosts)
	}
	return hosts
}

// thirdParty says whether a fetched reference goes to a host other than
// own: an absolute or scheme-relative http(s) URL elsewhere.
func thirdParty(ref string, own map[string]bool) bool {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "//") && !strings.HasPrefix(strings.ToLower(ref), "http:") && !strings.HasPrefix(strings.ToLower(ref), "https:") {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return true
	}
	return !own[strings.ToLower(u.Hostname())]
}

// cssLoads lists the third-party URLs a stylesheet loads.
func cssLoads(css string, own map[string]bool) []string {
	var found []string
	for _, m := range cssURLRE.FindAllStringSubmatch(css, -1) {
		ref := m[1] + m[2]
		if thirdParty(ref, own) {
			found = append(found, "css "+ref)
		}
	}
	return found
}

// tagLoads lists the third-party URLs one tag makes the browser fetch.
func tagLoads(tag, attrs string, own map[string]bool) []string {
	values := map[string]string{}
	for _, a := range attrRE.FindAllStringSubmatch(attrs, -1) {
		values[strings.ToLower(a[1])] = a[2] + a[3] + a[4]
	}
	if tag == "link" && slices.ContainsFunc(strings.Fields(strings.ToLower(values["rel"])), navLinkRel) {
		return nil
	}
	var found []string
	for _, name := range loadAttrs(tag) {
		refs := []string{values[name]}
		if name == "srcset" {
			refs = nil
			for c := range strings.SplitSeq(values[name], ",") {
				if f := strings.Fields(c); len(f) > 0 {
					refs = append(refs, f[0])
				}
			}
		}
		for _, ref := range refs {
			if thirdParty(ref, own) {
				found = append(found, "<"+tag+" "+name+"> "+ref)
			}
		}
	}
	return append(found, cssLoads(values["style"], own)...)
}

// pageLoads lists the third-party URLs an HTML page loads by itself: its
// tags' fetched attributes, its style attributes and its <style> blocks.
func pageLoads(html string, own map[string]bool) []string {
	var found []string
	for _, m := range tagRE.FindAllStringSubmatch(html, -1) {
		found = append(found, tagLoads(strings.ToLower(m[1]), m[2], own)...)
	}
	for _, m := range styleRE.FindAllStringSubmatch(html, -1) {
		found = append(found, cssLoads(m[1], own)...)
	}
	return found
}

// The built site makes no third-party requests: no page or stylesheet loads
// a script, style, font, image or frame from, or connects ahead to, a host
// other than the site's own. mise run lint:docs builds the site and runs
// this with DOCS_SITE set to the build's folder.
func TestNoThirdPartyLoads(t *testing.T) {
	site := os.Getenv("DOCS_SITE")
	if site == "" {
		t.Skip("DOCS_SITE isn't set: mise run lint:docs builds the site and runs this test on it")
	}
	own := ownHosts(t)
	files := 0
	err := filepath.WalkDir(site, func(path string, d fs.DirEntry, err error) error {
		ext := filepath.Ext(path)
		if err != nil || d.IsDir() || (ext != ".html" && ext != ".css") {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // the site this run built
		if err != nil {
			return err
		}
		files++
		loads := cssLoads(string(data), own)
		if ext == ".html" {
			loads = pageLoads(string(data), own)
		}
		name, _ := filepath.Rel(site, path)
		for _, l := range loads {
			t.Errorf("%s loads from another host: %s; serve it from the site instead", name, l)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("no HTML or CSS in %s", site)
	}
}

func TestPageLoads(t *testing.T) {
	own := map[string]bool{"devvm.dev": true, "next.devvm.dev": true}
	for _, tc := range []struct {
		html string
		want []string
	}{
		{`<link rel=stylesheet href="https://fonts.googleapis.com/css2?family=X">`, []string{"<link href> https://fonts.googleapis.com/css2?family=X"}},
		{`<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>`, []string{"<link href> https://fonts.gstatic.com"}},
		{`<link rel=dns-prefetch href=//cdn.example.com>`, []string{"<link href> //cdn.example.com"}},
		{`<link rel=canonical href=https://example.com/x>`, nil},
		{`<link rel=stylesheet href=/css/main.css><link rel=preload href=https://devvm.dev/f.woff2>`, nil},
		{`<SCRIPT defer SRC='https://cdn.jsdelivr.net/npm/x.js'></SCRIPT>`, []string{"<script src> https://cdn.jsdelivr.net/npm/x.js"}},
		{`<img alt="a > b" src=/a.png srcset="/a.png 1x, https://img.example.com/a@2x.png 2x">`, []string{"<img srcset> https://img.example.com/a@2x.png"}},
		{`<iframe src="https://www.youtube.com/embed/x"></iframe>`, []string{"<iframe src> https://www.youtube.com/embed/x"}},
		{`<div style="background: url('https://example.com/bg.png')">`, []string{"css https://example.com/bg.png"}},
		{`<style>@import "https://example.com/a.css"; @font-face { src: url(/fonts/a.woff2) }</style>`, []string{"css https://example.com/a.css"}},
		{`<a href="https://github.com/devvmhq/devvm">GitHub</a><meta property=og:image content="https://example.com/x.png">`, nil},
		{`<img src="data:image/png;base64,AAAA">`, nil},
	} {
		if got := pageLoads(tc.html, own); !slices.Equal(got, tc.want) {
			t.Errorf("pageLoads(%q) = %q, want %q", tc.html, got, tc.want)
		}
	}
}
