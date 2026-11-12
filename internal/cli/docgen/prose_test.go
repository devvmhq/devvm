package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The limits a user page's prose keeps to, which the docs-voice skill's
// Commands first on user pages explains: a run of paragraphs back to back
// may hold one long paragraph but not two, and no paragraph or list item
// grows into a wall. scan.sh -p paras and -p long show the same blocks.
const (
	longWords  = 80  // a paragraph over this is long
	runLen     = 3   // a run of this many paragraphs may hold one long one
	wallWords  = 150 // no paragraph or list item is over this
	leadInMost = 40  // a colon-ended paragraph of at most this introduces what follows
)

// userPageDirs are the sections whose pages are user pages.
func userPageDirs() []string {
	return []string{"getting-started", "using", "safety", "backends", "compared"}
}

// proseExempt are user pages that another branch is rewriting, each with
// why. Take a page out once its branch lands.
func proseExempt() map[string]string {
	return map[string]string{
		"using/clone.md":         "the clone stack rewrites it",
		"using/coding-agents.md": "the agents stack rewrites it",
	}
}

// A block of a page's prose: a paragraph (kind 'p') or a list item ('l').
type proseBlock struct {
	kind    byte
	line    int
	words   int
	leadIn  bool // a short paragraph that ends with a colon
	heading string
}

var (
	codeSpan   = regexp.MustCompile("`[^`]*`")
	linkTarget = regexp.MustCompile(`\]\([^)]*\)`)
	listStart  = regexp.MustCompile(`^\s*([-*+]|[0-9]+\.)\s`)
	otherStart = regexp.MustCompile(`^\s*([|><]|\{\{)`)
)

// proseWords counts a block's words as scan.sh does: a code span is one
// word, and a link counts its text alone.
func proseWords(text string) int {
	text = codeSpan.ReplaceAllString(text, "x")
	text = linkTarget.ReplaceAllString(text, "]")
	return len(strings.Fields(text))
}

// proseBlocks splits a page into its paragraphs and list items, with nil
// for each block that ends a run: a heading, a fence, a table, a callout,
// a shortcode or HTML. Front matter, fenced code and lines that are only
// an HTML comment are skipped.
func proseBlocks(page string) []*proseBlock {
	var (
		blocks  []*proseBlock
		cur     *proseBlock
		text    []string
		last    string
		other   bool
		fence   bool
		front   bool
		heading = "(no heading)"
	)
	end := func() {
		if cur != nil {
			cur.words = proseWords(strings.Join(text, " "))
			cur.leadIn = cur.kind == 'p' && strings.HasSuffix(strings.TrimSpace(last), ":") && cur.words <= leadInMost
			blocks = append(blocks, cur)
		}
		cur, text, other = nil, nil, false
	}
	start := func(kind byte, n int, line string) {
		end()
		cur = &proseBlock{kind: kind, line: n, heading: heading}
		text, last = []string{line}, line
	}
	for i, line := range strings.Split(page, "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		switch {
		case n == 1 && line == "---":
			front = true
		case front:
			front = line != "---"
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			end()
			blocks = append(blocks, nil)
			fence = !fence
		case fence:
		case strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->"):
		case trimmed == "":
			end()
		case listStart.MatchString(line):
			start('l', n, line)
		case cur != nil:
			text, last = append(text, line), line
		case other:
		case strings.HasPrefix(line, "#"):
			end()
			blocks = append(blocks, nil)
			heading = line
		case otherStart.MatchString(line):
			end()
			blocks = append(blocks, nil)
			other = true
		case strings.TrimLeft(line, " \t") != line:
			start('l', n, line)
		default:
			start('p', n, line)
		}
	}
	end()
	return blocks
}

// proseProblems lists what breaks the limits on one page.
func proseProblems(page string) []string {
	var problems, sizes []string
	var run []*proseBlock
	flush := func() {
		long := 0
		for _, b := range run {
			sizes = append(sizes, fmt.Sprint(b.words))
			if b.words > longWords {
				long++
			}
		}
		if len(run) >= runLen && long >= 2 {
			problems = append(problems, fmt.Sprintf("%d: %d paragraphs back to back (%s words) under %s, two of them over %d words: split them with a command, a list or a table, or move the mechanism to How it works",
				run[0].line, len(run), strings.Join(sizes, "+"), run[0].heading, longWords))
		}
		run, sizes = nil, nil
	}
	for _, b := range proseBlocks(page) {
		if b == nil || b.kind == 'l' {
			flush()
		}
		if b == nil {
			continue
		}
		if b.words > wallWords {
			what := "paragraph"
			if b.kind == 'l' {
				what = "list item"
			}
			problems = append(problems, fmt.Sprintf("%d: a %s of %d words under %s, over %d: split it, lead with its command, or move the mechanism to How it works",
				b.line, what, b.words, b.heading, wallWords))
		}
		if b.kind == 'p' && !b.leadIn {
			run = append(run, b)
		}
	}
	flush()
	return problems
}

// User pages lead with doing: no run of paragraphs with two long ones, and
// no paragraph or list item over the wall's limit.
func TestUserPageProse(t *testing.T) {
	exempt := proseExempt()
	pages := 0
	for _, dir := range userPageDirs() {
		err := filepath.WalkDir(filepath.Join(content, "docs", dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return err
			}
			name, _ := filepath.Rel(filepath.Join(content, "docs"), path)
			name = filepath.ToSlash(name)
			if _, ok := exempt[name]; ok {
				return nil
			}
			data, err := os.ReadFile(path) //nolint:gosec // the repo's own pages
			if err != nil {
				return err
			}
			pages++
			for _, p := range proseProblems(string(data)) {
				t.Errorf("%s:%s", name, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if pages == 0 {
		t.Fatalf("no user pages in %s", content)
	}
	for name := range exempt {
		if _, err := os.Stat(filepath.Join(content, "docs", name)); err != nil {
			t.Errorf("%s is exempt but isn't a page: take it out of proseExempt", name)
		}
	}
}

func TestProseProblems(t *testing.T) {
	long := strings.Repeat("word ", longWords+1)
	wall := strings.Repeat("word ", wallWords+1)
	for _, tc := range []struct {
		name string
		page string
		want int
	}{
		{"short paragraphs", "one.\n\ntwo.\n\nthree.\n", 0},
		{"one long in a run", long + "\n\nshort.\n\nshort.\n", 0},
		{"two long in a run of two", long + "\n\n" + long + "\n", 0},
		{"two long in a run of three", long + "\n\n" + long + "\n\nshort.\n\n" + long + "\n", 1},
		{"a list breaks the run", long + "\n\n- item\n\n" + long + "\n\n" + long + "\n", 0},
		{"a fence breaks the run", long + "\n\n```sh\nx\n```\n\n" + long + "\n\n" + long + "\n", 0},
		{"a heading breaks the run", long + "\n\n## h\n\n" + long + "\n\n" + long + "\n", 0},
		{"a short lead-in doesn't count", long + "\n\nTo do it:\n\n" + long + "\n", 0},
		{"a long lead-in counts", long + "\n\n" + long + " end:\n\nshort.\n", 1},
		{"a wall", wall + "\n", 1},
		{"a wall in a list item", "- " + wall + "\n", 1},
		{"a wall in a list item's lines", "- one\n  " + wall + "\n", 1},
		{"code spans are one word", "`" + wall + "`\n", 0},
		{"link targets don't count", "[a](" + strings.Repeat("x/", 200) + ")\n", 0},
		{"front matter and fences are skipped", "---\ntitle: " + wall + "\n---\n\n```\n" + wall + "\n```\n", 0},
		{"a table isn't prose", "| " + wall + " |\n", 0},
	} {
		if got := proseProblems(tc.page); len(got) != tc.want {
			t.Errorf("%s: %d problems %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}
