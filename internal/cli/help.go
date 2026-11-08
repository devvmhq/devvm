package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
)

// commandGroup is a section of the root's help: its title and the
// commands in it, by name, a root command's subcommand by both names
// (layer add).
type commandGroup struct {
	id, title string
	commands  []string
}

// commandGroups are the root's help's sections, in order. help is in
// Cobra's own section, Additional Commands. A group none of whose commands
// exist is left out. A subcommand in a group other than its parent's is
// listed in that one by both names, and its parent's help groups its
// subcommands the same way.
func commandGroups() []commandGroup {
	return []commandGroup{
		{"vm", "Your VM:", []string{"restart", "stop", "update", "rm", "adopt", "ls", "code", "desktop", "host", "history"}},
		{"change", "Change your VM:", []string{"tools", "size", "ports", "network", "ssh-agent", "git-protect", "clone", "secret", "layer add", "layer rm"}},
		{"links", "Links between VMs:", []string{"link", "group"}},
		{"images", "Images:", []string{"images", "build", "rebuild", "layer", "layer new"}},
		{"space", "Disk space:", []string{"space", "gc", "retention"}},
		{"machine", "This machine:", []string{"setup", "agent", "doctor", "upgrade", "completion", "limactl"}},
	}
}

// usageTemplate is Cobra's own, with each command's docs page at the end,
// and for the root, devvm's issue tracker.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{range $parent := $cmds}}{{if ne $parent.GroupID $group.ID}}{{range $parent.Commands}}{{if (and (eq .GroupID $group.ID) .IsAvailableCommand)}}
  {{rpad (print $parent.Name " " .Name) $parent.NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}{{with index .Annotations "docs"}}

Docs:
  {{.}}{{end}}{{with index .Annotations "issues"}}

Issues:
  {{.}}{{end}}
`

// addGroups puts root's commands into commandGroups' sections, and a
// subcommand listed by both names into its parent's section of the same
// group.
func addGroups(root *cobra.Command) {
	for _, g := range commandGroups() {
		var cmds []*cobra.Command
		for _, path := range g.commands {
			if c, _, err := root.Find(strings.Fields(path)); err == nil && c != root && c.CommandPath() == root.Name()+" "+path {
				cmds = append(cmds, c)
			}
		}
		for _, c := range cmds {
			if !c.Parent().ContainsGroup(g.id) {
				c.Parent().AddGroup(&cobra.Group{ID: g.id, Title: g.title})
			}
			c.GroupID = g.id
		}
	}
}

// addDocs ends the help of root and every command under it with its docs
// page, from msg's links, and the root's with the issue tracker.
func addDocs(root *cobra.Command) {
	root.SetUsageTemplate(usageTemplate)
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations["issues"] = msg.IssuesURL
	setDocs(root)
}

func setDocs(c *cobra.Command) {
	if u := msg.CommandDocURL(c.CommandPath()); u != "" {
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations["docs"] = u
	}
	for _, sub := range c.Commands() {
		setDocs(sub)
	}
}

// orList is a list of names in a help's prose: a, b or c.
func orList(names []string) string { return joinLast(names, " or ") }

// andList is a list of names in a help's prose: a, b and c.
func andList(names []string) string { return joinLast(names, " and ") }

// joinLast joins names with commas, last before the last of them.
func joinLast(names []string, last string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + last + names[len(names)-1]
}

// possessive is each name with 's: pi's.
func possessive(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + "'s"
	}
	return out
}

// examples is a command's Example: each pair of arguments is a command
// line and a short comment on it, and the comments line up.
func examples(pairs ...string) string {
	if len(pairs)%2 != 0 {
		panic("examples: a command line without its comment")
	}
	width := 0
	for i := 0; i < len(pairs); i += 2 {
		width = max(width, len(pairs[i]))
	}
	lines := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		line, comment := pairs[i], pairs[i+1]
		lines = append(lines, "  "+line+strings.Repeat(" ", width-len(line))+"  # "+comment)
	}
	return strings.Join(lines, "\n")
}
