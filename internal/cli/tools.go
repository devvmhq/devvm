package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
)

// newToolsCmd is devvm tools: this folder's VM's shared tools, and add,
// rm and update, which change them.
func newToolsCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Show or choose this folder's VM's shared tools",
		Long: "In a terminal, choose the toolchains and coding agents of this folder's VM in a list, which shows its layers too, then open a shell in it. " +
			"Without a terminal, or with --json, show its shared tools, each with its version and any newer version in devvm's tools lock, " +
			"which changes nothing, starts no VM and doesn't go online.",
		Example: examples(
			"devvm tools", "choose the VM's shared tools, or show them without a terminal",
			"devvm tools add python claude", "add python and Claude Code to the VM",
			"devvm tools update", "move the VM to the newest versions",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON && u.CanPick() {
				return a.env(func(env *vm.Env) error { return vm.Tools{Env: env}.Pick(cmd.Context(), vm.Options{}) }, msg.UpFailed)
			}
			var ts []tools.VMTool
			if err := a.env(func(env *vm.Env) error {
				var err error
				ts, err = vm.Tools{Env: env}.List(cmd.Context())
				return err
			}, msg.ToolsFailed); err != nil {
				return err
			}
			if asJSON {
				return printJSON(u, toolsJSON(ts))
			}
			return writeTools(u.Table(), ts)
		},
	}
	jsonFlag(cmd, &asJSON)
	cmd.AddCommand(newToolsAddCmd(a), newToolsRmCmd(a), newToolsUpdateCmd(a))
	return addFolderFlags(cmd, a)
}

// toolsOutput is devvm tools --json: the VM's shared tools, each with the
// newer version the tools lock has, when it has one, and whether that one
// is built.
type toolsOutput struct {
	Tools []vmToolJSON `json:"tools"`
}

type vmToolJSON struct {
	Tool    string `json:"tool"`
	Version string `json:"version"`
	Newer   string `json:"newer,omitempty"`
	Built   *bool  `json:"newer_built,omitempty"`
}

func toolsJSON(ts []tools.VMTool) toolsOutput {
	out := toolsOutput{Tools: make([]vmToolJSON, 0, len(ts))}
	for _, t := range ts {
		j := vmToolJSON{Tool: t.Tool, Version: t.Version, Newer: t.Newer}
		if t.Newer != "" {
			j.Built = &t.Built
		}
		out.Tools = append(out.Tools, j)
	}
	return out
}

// writeTools writes a VM's tools as a table, a header first.
func writeTools(tab *ui.Table, ts []tools.VMTool) error {
	tab.Header("TOOL", "VERSION", "NEWER", "BUILT")
	for _, t := range ts {
		built := ""
		if t.Newer != "" {
			built = "no"
			if t.Built {
				built = "yes"
			}
		}
		tab.Row(cell(t.Tool), cell(t.Version), cell(t.Newer), cell(built))
	}
	return tab.Flush()
}

func newToolsAddCmd(a *app) *cobra.Command {
	return addFolderFlags(&cobra.Command{
		Use:   "add TOOL...",
		Short: "Add shared tools to this folder's VM, then open a shell in it",
		Long: "Add shared tools to this folder's VM, then open a shell in it. " +
			"TOOL is a toolchain, such as node, or a coding agent, such as claude, each a separate word. " +
			"devvm builds any of them that aren't built, and for a new agent in a running VM it asks before restarting the VM.",
		Example: examples(
			"devvm tools add claude", "add Claude Code to the VM",
			"devvm tools add python codex", "add python and Codex",
		),
		Args: toolNames("add"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Tools{Env: env}.Add(cmd.Context(), vm.Options{}, args) }, msg.UpFailed)
		},
		ValidArgsFunction: completeToolNames,
	}, a)
}

func newToolsRmCmd(a *app) *cobra.Command {
	return addFolderFlags(&cobra.Command{
		Use:   "rm TOOL...",
		Short: "Take shared tools out of this folder's VM, then open a shell in it",
		Long: "Take shared tools out of this folder's VM, each TOOL a separate word, then open a shell in it. " +
			"What the VM installed itself stays.",
		Example: examples(
			"devvm tools rm python", "take python out of the VM",
			"devvm tools rm python claude", "take out python and Claude Code",
		),
		Args: toolNames("rm"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Tools{Env: env}.Remove(cmd.Context(), vm.Options{}, args) }, msg.UpFailed)
		},
		ValidArgsFunction: completeToolNames,
	}, a)
}

func newToolsUpdateCmd(a *app) *cobra.Command {
	return addFolderFlags(&cobra.Command{
		Use:   "update",
		Short: "Move this folder's VM to the newest shared tools, then open a shell in it",
		Long: "Check for newer versions of the shared tools, build them, and move this folder's VM to them, " +
			"then open a shell in it. Nothing in the VM's disk changes, because its tools are mounted.",
		Example: examples(
			"devvm tools update", "move the VM to the newest shared tools",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.env(func(env *vm.Env) error { return vm.Tools{Env: env}.Update(cmd.Context(), vm.Options{}) }, msg.UpFailed)
		},
	}, a)
}

// toolNames takes one or more TOOLs, each a word of its own: one joined
// with + or a comma is refused, naming sub, the devvm tools command (add,
// rm). add takes only shared tools' names.
func toolNames(sub string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := minArgs(1)(cmd, args); err != nil {
			return err
		}
		names := slices.Concat(image.Toolchains, image.Agents)
		for _, a := range args {
			if strings.ContainsAny(a, "+,") {
				return msg.ToolsJoined(sub, a)
			}
			if sub == "add" && !slices.Contains(names, a) {
				return msg.UnknownTool(a, strings.Join(names, " "))
			}
		}
		return nil
	}
}
