package cli

import (
	"io/fs"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

// A DocBlock is a part of a hand-written page that mise run cli-docs
// writes from the registries, between its markers, so the page can't go
// stale: docgen writes it, and TestReferenceIsCurrent fails on a stale one.
type DocBlock struct {
	// Page is the page's path, from the repo's root.
	Page string
	// Name names the block in its markers.
	Name string
	// From is what its marker says it's generated from.
	From string
	// Text is the block's text, between its markers, from devvm's own
	// files (assets.FS).
	Text func(own fs.FS) (string, error)
}

// Begin is the marker the block's text starts after.
func (b DocBlock) Begin() string {
	return "<!-- " + b.Name + ", generated from " + b.From + " by mise run cli-docs -->"
}

// End is the marker the block's text ends before.
func (b DocBlock) End() string { return "<!-- end of " + b.Name + " -->" }

// DocBlocks are the pages' generated blocks: the docs' Supported agents
// table, the README's list of the agents, and the agents' history paths,
// which an export holds and an import allows.
func DocBlocks() []DocBlock {
	return []DocBlock{
		{
			Page: "docs/content/docs/using/coding-agents.md", Name: "the agents' table",
			From: "internal/agents and the hosts in assets/shared-tools/<name>.policy", Text: agentsTable,
		},
		{Page: "README.md", Name: "the agents' names", From: "internal/agents", Text: func(fs.FS) (string, error) {
			var names []string
			for _, d := range agents.Defs {
				names = append(names, d.Display)
			}
			return andList(names), nil
		}},
		{Page: "docs/content/docs/using/coding-agents.md", Name: "the agents' history", From: "internal/agents", Text: historyList},
		{Page: "docs/content/docs/internals/agents.md", Name: "the agents' history", From: "internal/agents", Text: historyList},
	}
}

// historyList is every agent's history paths in the VM's home, in the
// registry's order (agents.History).
func historyList(fs.FS) (string, error) {
	var paths []string
	for _, p := range agents.History() {
		paths = append(paths, "`~/"+p+"`")
	}
	return andList(paths), nil
}

// agentsTable is the Supported agents table: a row for each agent, in the
// registry's order, with the hosts its policy allows.
func agentsTable(own fs.FS) (string, error) {
	var s strings.Builder
	s.WriteString("\n\n| Name | Agent | What devvm adds | Allowlist, on the allowlist and none tiers |\n| --- | --- | --- | --- |\n")
	for _, d := range agents.Defs {
		policy, err := fs.ReadFile(own, "shared-tools/"+d.Name+".policy")
		if err != nil {
			return "", err
		}
		var hosts []string
		for _, l := range proxy.ParsePolicy(string(policy)) {
			hosts = append(hosts, "`"+l.Text+"`")
		}
		name := d.Display
		if d.Docs.Site != "" {
			name = "[" + name + "](" + d.Docs.Site + ")"
		}
		s.WriteString("| `" + d.Name + "` | " + name + " | " + d.Docs.Adds + " | " + d.Docs.Reaches + ": " + andList(hosts) + " |\n")
	}
	s.WriteString("\n")
	return s.String(), nil
}
