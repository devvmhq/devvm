package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// content is the docs site's pages, from this package's folder.
const content = "../../../docs/content"

// The social card's lines (docs/layouts/_partials/og-image.html): the
// card's text is 1024 pixels wide, and Departure Mono's characters are
// 7/11 of its size, so the explainer at 33px holds 48 characters a line
// and the title at its smallest, 44px, holds 36. Each gets two lines.
const (
	explainerCols = 48
	titleCols     = 36
	cardLines     = 2
)

// frontMatter returns the top-level scalar fields of a page's YAML front
// matter, unquoted. Lists, maps and comments are skipped.
func frontMatter(data []byte) map[string]string {
	fields := map[string]string{}
	s := bufio.NewScanner(bytes.NewReader(data))
	if !s.Scan() || s.Text() != "---" {
		return fields
	}
	for s.Scan() {
		line := s.Text()
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || strings.ContainsAny(key[:1], " \t#-") {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, `"`):
			if v, err := strconv.Unquote(value); err == nil {
				value = v
			}
		case strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) > 1:
			value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		}
		fields[key] = value
	}
	return fields
}

// wrappedLines is how many lines text takes when wrapped at spaces into
// lines of at most cols characters, as og-wrap.html wraps it.
func wrappedLines(text string, cols int) int {
	lines, width := 0, 0
	for word := range strings.FieldsSeq(text) {
		n := utf8.RuneCountInString(word)
		if width > 0 && width+1+n <= cols {
			width += 1 + n
			continue
		}
		lines++
		width = n
	}
	return lines
}

// Every page has an explainer, the short line under its title on its
// social card, and the explainer and the card's title each fit the card.
// The CLI reference's explainers are its commands' Shorts.
func TestExplainers(t *testing.T) {
	pages := 0
	err := filepath.WalkDir(content, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // the repo's own pages
		if err != nil {
			return err
		}
		pages++
		name, _ := filepath.Rel(content, path)
		fields := frontMatter(data)
		explainer := fields["explainer"]
		switch {
		case explainer == "":
			t.Errorf("%s: no explainer in its front matter: add one, a few plain words on what the page covers", name)
		case wrappedLines(explainer, explainerCols) > cardLines:
			t.Errorf("%s: explainer %q is longer than the card's %d lines of %d characters", name, explainer, cardLines, explainerCols)
		}
		title := fields["cardTitle"]
		if title == "" {
			title = fields["title"]
		}
		if wrappedLines(title, titleCols) > cardLines {
			t.Errorf("%s: title %q is longer than the card's %d lines of %d characters", name, title, cardLines, titleCols)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatalf("no pages in %s", content)
	}
}

func TestFrontMatter(t *testing.T) {
	page := "---\ntitle: Toolchains\nweight: 3\nexplainer: \"Say \\\"hi\\\"\"\nalt: 'it''s'\n# note: no\nsidebar:\n  exclude: true\n---\n\nbody: no\n"
	got := frontMatter([]byte(page))
	want := map[string]string{"title": "Toolchains", "weight": "3", "explainer": `Say "hi"`, "alt": "it's", "sidebar": ""}
	if len(got) != len(want) {
		t.Fatalf("frontMatter = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("frontMatter[%q] = %q, want %q", k, got[k], v)
		}
	}
	if got := frontMatter([]byte("no front matter\n")); len(got) != 0 {
		t.Errorf("frontMatter of a page without one = %v, want none", got)
	}
}

func TestWrappedLines(t *testing.T) {
	for _, tc := range []struct {
		text string
		cols int
		want int
	}{
		{"", 10, 0},
		{"one", 10, 1},
		{"one two", 7, 1},
		{"one two", 6, 2},
		{"a looooooooong word", 5, 3},
		{"  spaced   out  ", 10, 1},
		{"Run Lima's limactl for debugging, which starts VMs unconfined, whatever their tier", 48, 2},
	} {
		if got := wrappedLines(tc.text, tc.cols); got != tc.want {
			t.Errorf("wrappedLines(%q, %d) = %d, want %d", tc.text, tc.cols, got, tc.want)
		}
	}
}
