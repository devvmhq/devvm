package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// changelogFile is the repo's CHANGELOG.md, from this package's folder.
const changelogFile = "../../../CHANGELOG.md"

// The shape of a changelog entry, which Contributing's Releasing explains:
// entries from shapedFrom on are sorted by who notices each change, and the
// ones before it are frozen history. Highlights holds three to five lines,
// and every line but a step in What to do keeps its prose, its links and a
// credit left out, to lineWords words.
const (
	shapedFrom    = "0.2.0-rc.1"
	highlightsMin = 3
	highlightsMax = 5
	lineWords     = 35
)

// The groups of an entry, in their order. If you use… may repeat, one
// block for each feature, and every other group appears once at most.
const (
	groupHighlights = iota
	groupWhatToDo
	groupFixed
	groupIfYouUse
	groupHardening
	groupInternal
)

// groupHeadings are the groups that are headings, and groupSummaries the
// start of each collapsed group's summary.
func groupHeadings() map[string]int {
	return map[string]int{
		"### Highlights": groupHighlights,
		"### What to do": groupWhatToDo,
		"### Fixed":      groupFixed,
	}
}

func groupSummaries() map[string]int {
	return map[string]int{
		"If you use ":         groupIfYouUse,
		"Security hardening:": groupHardening,
		"Internal changes":    groupInternal,
	}
}

var (
	entryHeading = regexp.MustCompile(`^## (.+)$`)
	detailsOpen  = regexp.MustCompile(`^<details><summary>(.+)</summary>$`)
	bulletLine   = regexp.MustCompile(`^- \S`)
	stepLine     = regexp.MustCompile(`^[0-9]+\. \S`)
	mdLink       = regexp.MustCompile(`\[([^\]]*)\]\(([^)]*)\)`)
	issueText    = regexp.MustCompile(`^#([0-9]+)$`)
	bareIssue    = regexp.MustCompile(`#[0-9]`)
	detailsLink  = regexp.MustCompile(`\[details\]\(https://(next\.)?devvm\.dev/docs/releases/[^)]+\)`)
	trailLinks   = regexp.MustCompile(`\s*\((\[[^\]]*\]\([^)]*\)(,\s*)?)+\)\.?$`)
	credit       = regexp.MustCompile(`\s*Thanks to .*$`)
)

// changelogEntry is one release's entry, or Unreleased's, with the number
// of its heading's line.
type changelogEntry struct {
	version string
	line    int
	lines   []string
}

// changelogEntries splits a changelog into its entries, in the file's order.
func changelogEntries(text string) []changelogEntry {
	var entries []changelogEntry
	for i, line := range strings.Split(text, "\n") {
		if m := entryHeading.FindStringSubmatch(line); m != nil {
			entries = append(entries, changelogEntry{version: m[1], line: i + 1})
			continue
		}
		if len(entries) > 0 {
			e := &entries[len(entries)-1]
			e.lines = append(e.lines, line)
		}
	}
	return entries
}

// shapedEntries are the entries the shape applies to: each one from the
// top down to shapedFrom, and Unreleased once it has a line.
func shapedEntries(t *testing.T, entries []changelogEntry) []changelogEntry {
	t.Helper()
	var shaped []changelogEntry
	newer := true
	for _, e := range entries {
		switch {
		case e.version == "Unreleased":
			if hasLines(e.lines) {
				shaped = append(shaped, e)
			}
		case newer:
			shaped = append(shaped, e)
		}
		if e.version == shapedFrom {
			newer = false
		}
	}
	if newer {
		t.Fatalf("CHANGELOG.md has no entry ## %s", shapedFrom)
	}
	return shaped
}

func hasLines(lines []string) bool {
	for _, l := range lines {
		if bulletLine.MatchString(l) || stepLine.MatchString(l) {
			return true
		}
	}
	return false
}

// entryChecker walks one entry's lines, keeping which group it's in.
type entryChecker struct {
	e        changelogEntry
	problems []string
	group    int  // the group the walk is in, or -1 before the first
	open     bool // inside a <details> block
	counts   map[int]int
	seen     map[int]bool
}

func (c *entryChecker) fail(i int, format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf("%d: ## %s: %s", c.e.line+1+i, c.e.version, fmt.Sprintf(format, args...)))
}

// enter starts a group, which comes after the one before it in the order
// and only once, but for If you use….
func (c *entryChecker) enter(i, g int, name string) {
	if g < c.group || (g == c.group && g != groupIfYouUse) || (c.seen[g] && g != groupIfYouUse) {
		c.fail(i, "%s is out of order or repeated: the groups are Highlights, What to do, Fixed, If you use… blocks, Security hardening, then Internal", name)
	}
	c.group = g
	c.seen[g] = true
}

// changelogProblems lists what breaks an entry's shape. A released entry
// needs its Highlights, and Unreleased gets them at the release.
func changelogProblems(e changelogEntry) []string {
	c := &entryChecker{e: e, group: -1, counts: map[int]int{}, seen: map[int]bool{}}
	headings, summaries := groupHeadings(), groupSummaries()
	for i, line := range e.lines {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "#"):
			g, ok := headings[line]
			if !ok || c.open {
				c.fail(i, "%q isn't one of the groups' headings, or it's inside a <details> block", line)
				continue
			}
			c.enter(i, g, line)
		case strings.HasPrefix(line, "<details>"):
			c.openDetails(i, line, summaries)
		case line == "</details>":
			c.closeDetails(i)
		case bulletLine.MatchString(line) || stepLine.MatchString(line):
			c.checkLine(i, line)
		case c.group >= 0:
			c.fail(i, "only a group's lines go in a group, one line each: %q", line)
		}
	}
	if c.open {
		c.fail(len(e.lines)-1, "a <details> block has no </details>")
	}
	for g, name := range map[int]string{groupHighlights: "Highlights", groupWhatToDo: "What to do", groupFixed: "Fixed", groupHardening: "Security hardening", groupInternal: "Internal"} {
		if c.seen[g] && c.counts[g] == 0 {
			c.fail(0, "%s has no lines", name)
		}
	}
	n := c.counts[groupHighlights]
	if (e.version != "Unreleased" || c.seen[groupHighlights]) && (n < highlightsMin || n > highlightsMax) {
		c.fail(0, "Highlights has %d lines, where it needs %d to %d", n, highlightsMin, highlightsMax)
	}
	return c.problems
}

// openDetails starts a collapsed group, whose <summary> is on the same line
// and whose next line is blank, so GitHub renders the list inside.
func (c *entryChecker) openDetails(i int, line string, summaries map[string]int) {
	m := detailsOpen.FindStringSubmatch(line)
	if m == nil || c.open {
		c.fail(i, "a collapsed group is one line, <details><summary>…</summary>, outside any other: %q", line)
		return
	}
	c.open = true
	g := -1
	for prefix, sg := range summaries {
		if strings.HasPrefix(m[1], prefix) {
			g = sg
		}
	}
	if g < 0 {
		c.fail(i, "%q isn't a collapsed group's summary: If you use …, Security hardening: … or Internal changes", m[1])
	} else {
		c.enter(i, g, m[1])
	}
	if i+1 >= len(c.e.lines) || strings.TrimSpace(c.e.lines[i+1]) != "" {
		c.fail(i, "a blank line follows <summary>, or GitHub doesn't render the list")
	}
}

func (c *entryChecker) closeDetails(i int) {
	if !c.open {
		c.fail(i, "</details> without its <details>")
		return
	}
	c.open = false
	if i == 0 || strings.TrimSpace(c.e.lines[i-1]) != "" {
		c.fail(i, "a blank line comes before </details>, or GitHub doesn't render the list")
	}
}

// checkLine checks one line of a group: in a group, in its list's form, a
// details link to releases/, full links for issues, and its words.
func (c *entryChecker) checkLine(i int, line string) {
	switch {
	case c.group < 0:
		c.fail(i, "a line before the first group")
		return
	case c.group >= groupIfYouUse && !c.open:
		c.fail(i, "a line after a collapsed group, outside any group")
		return
	case c.group == groupWhatToDo && !stepLine.MatchString(line):
		c.fail(i, "What to do's lines are numbered steps")
	case c.group != groupWhatToDo && !bulletLine.MatchString(line):
		c.fail(i, "only What to do's lines are numbered")
	}
	c.counts[c.group]++
	if !detailsLink.MatchString(line) {
		c.fail(i, "the line has no [details](…) link to its section on a releases/ page")
	}
	for _, m := range mdLink.FindAllStringSubmatch(line, -1) {
		if n := issueText.FindStringSubmatch(m[1]); n != nil && m[2] != "https://github.com/devvmhq/devvm/issues/"+n[1] && m[2] != "https://github.com/devvmhq/devvm/pull/"+n[1] {
			c.fail(i, "[%s] links %s, not its issue or pull request on github.com/devvmhq/devvm", m[1], m[2])
		}
	}
	if bareIssue.MatchString(mdLink.ReplaceAllString(codeSpan.ReplaceAllString(line, ""), "")) {
		c.fail(i, "a bare #N: write an issue as [#N](https://github.com/devvmhq/devvm/issues/N)")
	}
	if c.group == groupWhatToDo {
		return
	}
	prose := credit.ReplaceAllString(trailLinks.ReplaceAllString(line, ""), "")
	if w := proseWords(prose); w > lineWords {
		c.fail(i, "the line's prose is %d words, over %d: one outcome sentence, with the rest on the release page", w, lineWords)
	}
}

// Each entry from shapedFrom on, and Unreleased once it has a line, is
// sorted by audience in its groups, in their order and shape.
func TestChangelogShape(t *testing.T) {
	data, err := os.ReadFile(changelogFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range shapedEntries(t, changelogEntries(string(data))) {
		for _, p := range changelogProblems(e) {
			t.Errorf("CHANGELOG.md:%s", p)
		}
	}
}

// Each of the shape's rules fails an entry that breaks it, and a good one
// passes.
func TestChangelogProblems(t *testing.T) {
	const (
		r     = "https://next.devvm.dev/docs/releases/0.2.0/#x"
		links = " ([details](" + r + "), [abc1234](https://github.com/devvmhq/devvm/commit/abc1234))."
		hl    = "### Highlights\n\n- One" + links + "\n- Two" + links + "\n- Three" + links + "\n"
	)
	long := "- " + strings.Repeat("word ", lineWords+1) + links
	for name, tc := range map[string]struct {
		version, body string
		want          string // part of the one problem, or "" for none
	}{
		"good": {"1.0.0", "Intro.\n\n" + hl + "\n### What to do\n\n1. Run `devvm update`" + links + "\n\n### Fixed\n\n- A fix ([details](" + r + "), [#5](https://github.com/devvmhq/devvm/issues/5)). Thanks to @a for the report.\n\n" +
			"<details><summary>If you use a Linux host</summary>\n\n- A" + links + "\n\n</details>\n<details><summary>If you use Cursor</summary>\n\n- B" + links + "\n\n</details>\n" +
			"<details><summary>Security hardening: less</summary>\n\n- C" + links + "\n\n</details>\n<details><summary>Internal changes</summary>\n\n- D" + links + "\n\n</details>\n", ""},
		"unreleased without highlights": {"Unreleased", "### Fixed\n\n- A" + links + "\n", ""},
		"too few highlights":            {"1.0.0", "### Highlights\n\n- One" + links + "\n", "Highlights has 1 lines"},
		"too many highlights":           {"1.0.0", hl + "- Four" + links + "\n- Five" + links + "\n- Six" + links + "\n", "Highlights has 6 lines"},
		"no highlights":                 {"1.0.0", "### Fixed\n\n- A" + links + "\n", "Highlights has 0 lines"},
		"old heading":                   {"1.0.0", hl + "### Breaking\n\n- A" + links + "\n", "isn't one of the groups' headings"},
		"out of order":                  {"1.0.0", hl + "### Fixed\n\n- A" + links + "\n### What to do\n\n1. B" + links + "\n", "out of order"},
		"unknown summary":               {"1.0.0", hl + "<details><summary>Other</summary>\n\n- A" + links + "\n\n</details>\n", "isn't a collapsed group's summary"},
		"unclosed details":              {"1.0.0", hl + "<details><summary>Internal changes</summary>\n\n- A" + links + "\n", "has no </details>"},
		"no blank after summary":        {"1.0.0", hl + "<details><summary>Internal changes</summary>\n- A" + links + "\n\n</details>\n", "a blank line follows <summary>"},
		"no blank before close":         {"1.0.0", hl + "<details><summary>Internal changes</summary>\n\n- A" + links + "\n</details>\n", "a blank line comes before </details>"},
		"line after a block":            {"1.0.0", hl + "<details><summary>Internal changes</summary>\n\n- A" + links + "\n\n</details>\n- B" + links + "\n", "outside any group"},
		"unnumbered step":               {"1.0.0", hl + "### What to do\n\n- A" + links + "\n", "numbered steps"},
		"numbered fix":                  {"1.0.0", hl + "### Fixed\n\n1. A" + links + "\n", "only What to do's lines are numbered"},
		"no details link":               {"1.0.0", hl + "### Fixed\n\n- A ([abc1234](https://github.com/devvmhq/devvm/commit/abc1234)).\n", "no [details](…) link"},
		"bare issue":                    {"1.0.0", hl + "### Fixed\n\n- A, as #12 reported" + links + "\n", "a bare #N"},
		"issue linked elsewhere":        {"1.0.0", hl + "### Fixed\n\n- A ([details](" + r + "), [#12](https://example.com/12)).\n", "not its issue or pull request"},
		"long line":                     {"1.0.0", hl + "### Fixed\n\n" + long + "\n", "words, over"},
		"long step":                     {"1.0.0", hl + "### What to do\n\n1. " + strings.TrimPrefix(long, "- ") + "\n", ""},
		"prose in a group":              {"1.0.0", hl + "### Fixed\n\nSome prose.\n\n- A" + links + "\n", "only a group's lines"},
		"line before a group":           {"1.0.0", "- A" + links + "\n" + hl, "a line before the first group"},
	} {
		got := changelogProblems(changelogEntry{version: tc.version, line: 1, lines: strings.Split(tc.body, "\n")})
		switch {
		case tc.want == "" && len(got) > 0:
			t.Errorf("%s: problems %q, want none", name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: problems %q, want one with %q", name, got, tc.want)
		}
	}
}
