package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// write replaces the Markdown pages in dir with root's reference. dir is
// the developer's argument, so the paths under it are trusted.
func write(root *cobra.Command, dir string) error {
	files, err := generate(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the developer's folder
		return err
	}
	old, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	for _, f := range old {
		if err := os.Remove(f); err != nil { //nolint:gosec // a page in the developer's folder
			return err
		}
	}
	for name, data := range files {
		if err := state.WriteFile(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// generate returns the reference's files by name: _index.md, and a page
// for root and each command under it users can run. Hidden commands and
// Cobra's help command are left out, as Cobra's own tree generator does.
// Each page's front matter has its explainer, which TestExplainers checks.
func generate(root *cobra.Command) (map[string][]byte, error) {
	cmds := available(root)
	files := make(map[string][]byte, len(cmds)+1)
	var index bytes.Buffer
	index.WriteString("---\ntitle: CLI reference\nweight: 90\nexplainer: Every devvm command, with its arguments and flags\n---\n\n" +
		"Every devvm command, with its arguments and flags. `devvm <command> --help` prints the same.\n\n")
	for i, c := range cmds {
		c.DisableAutoGenTag = true
		var md bytes.Buffer
		if err := doc.GenMarkdownCustom(c, &md, func(s string) string { return s }); err != nil {
			return nil, err
		}
		body := withDocs(tidy(md.String()), c.CommandPath(), msg.CommandDoc(c.CommandPath()))
		// The command's Short is its page's explainer, the line under its title on the social card.
		page := fmt.Sprintf("---\ntitle: %s\nweight: %d\nexplainer: %q\n---\n\n%s", c.CommandPath(), i+1, c.Short, body)
		files[fileName(c)] = []byte(page)
		fmt.Fprintf(&index, "- [%s](%s): %s\n", c.CommandPath(), fileName(c), c.Short)
	}
	files["_index.md"] = index.Bytes()
	return files, nil
}

// tidy fits Cobra's Markdown to the site's pages: the title is in the
// front matter, so its heading goes and the others move up a level; code
// blocks say they're text; lists use - and no tabs; one newline at the end.
func tidy(md string) string {
	lines := strings.Split(md, "\n")
	if strings.HasPrefix(lines[0], "## ") {
		lines = lines[1:]
		for len(lines) > 0 && lines[0] == "" {
			lines = lines[1:]
		}
	}
	inCode := false
	for i, l := range lines {
		switch {
		case l == "```":
			if !inCode {
				lines[i] = "```text"
			}
			inCode = !inCode
		case inCode:
		case l == "### SEE ALSO":
			lines[i] = "## See also"
		case strings.HasPrefix(l, "### "):
			lines[i] = l[1:]
		case strings.HasPrefix(l, "* "):
			lines[i] = "- " + strings.Replace(l[2:], "\t - ", ": ", 1)
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

// withDocs adds a Docs section to a tidied page, before See also, linking
// to doc ("page#anchor", as msg has it) on the site; a page without a doc
// is left as it is.
func withDocs(md, path, doc string) string {
	if doc == "" {
		return md
	}
	page, anchor, _ := strings.Cut(doc, "#")
	target := "../" + page + ".md"
	if anchor != "" {
		target += "#" + anchor
	}
	section := "## Docs\n\n[The docs page for " + path + "](" + target + ") explains it in full.\n\n"
	if before, after, ok := strings.Cut(md, "## See also\n"); ok {
		return before + section + "## See also\n" + after
	}
	return md + "\n" + section[:len(section)-1]
}

// available returns c and the commands under it users can run, depth
// first, each command's children in Cobra's order.
func available(c *cobra.Command) []*cobra.Command {
	cmds := []*cobra.Command{c}
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() && !sub.IsAdditionalHelpTopicCommand() {
			cmds = append(cmds, available(sub)...)
		}
	}
	return cmds
}

// fileName is the page's file, as Cobra names it in its links.
func fileName(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "_") + ".md"
}
