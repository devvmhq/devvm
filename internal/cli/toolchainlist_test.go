package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/image"
)

// listedToolchains are the files that may list every toolchain: the
// toolchains page, which owns the list, and the two home pages, where it
// helps a visitor decide. A list about something else, such as what an
// e2e image holds, gets its file here with why.
func listedToolchains() []string {
	return []string{
		"docs/content/docs/using/toolchains.md",
		"docs/content/_index.md",
		"README.md",
		// the e2e images' contents and the e2e sections' tools, not the
		// toolchains' list
		"docs/content/docs/using/from.md",
		"docs/content/docs/contributing/_index.md",
		// design notes on the shared tools' mechanisms
		"docs/content/docs/internals/design.md",
	}
}

// sentencesOf is text's sentences: each line's, split at sentenceEnd.
func sentencesOf(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		out = append(out, sentenceEnd.Split(line, -1)...)
	}
	return out
}

// The toolchains are listed once, on the toolchains page (and the home
// pages): a sentence elsewhere in the docs, the in-VM guide, the messages
// or the issue forms that names three or more of image.Toolchains links to
// that page instead, so a new toolchain changes one list.
func TestToolchainsListedOnce(t *testing.T) {
	var files []string
	for _, dir := range []string{"docs/content", "assets/guest", ".github/ISSUE_TEMPLATE"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "cli" {
				return filepath.SkipDir // generated from the help
			}
			if !d.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".yml")) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	files = append(files, filepath.Join(repoRoot, "locales", "en.toml"))
	names := make([]*regexp.Regexp, len(image.Toolchains))
	for i, n := range image.Toolchains {
		// the name as a word, not part of a file's name such as
		// .node-version, go.mod or rust-toolchain.toml
		names[i] = regexp.MustCompile(`(?:^|[^\w./-])` + regexp.QuoteMeta(n) + `(?:$|[^\w./-]|\.\s|\.$)`)
	}
	for _, path := range files {
		rel, _ := filepath.Rel(repoRoot, path)
		if slices.Contains(listedToolchains(), filepath.ToSlash(rel)) {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304 the repo's own files
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sentencesOf(string(data)) {
			n := 0
			for _, re := range names {
				if re.MatchString(s) {
					n++
				}
			}
			if n >= 3 {
				t.Errorf("%s lists the toolchains (%q): link the toolchains page instead, or give one example", rel, strings.TrimSpace(s))
			}
		}
	}
}
