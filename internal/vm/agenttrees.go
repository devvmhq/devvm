package vm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// treePattern is where one of a VM's agents makes git worktrees
// (agents.Def.Worktrees): the agent's name as people know it, and the
// pattern of each worktree's folder, relative to the home folder.
type treePattern struct {
	agent, pattern string
}

// treePatterns are the Worktrees of the agents among st's shared tools,
// in their order: none for a VM whose creation didn't finish, which
// never ran.
func treePatterns(st *state.VM) []treePattern {
	if !st.Finished() {
		return nil
	}
	var out []treePattern
	for _, n := range image.AgentsOf(tools.Names(st)) {
		d := agents.Of(n)
		for _, p := range d.Worktrees {
			out = append(out, treePattern{agent: d.Display, pattern: p})
		}
	}
	return out
}

// treeGlobs are the shell's globs for pats, in order, and the pattern each
// stands for (of), whose index a record of agentTreesState names: each *
// of a pattern three ways, *, .[!.]* and ..?*, as a shell's * never
// matches a name that starts with a dot, which path.Match's does, so a
// worktree whose name does is found too.
func treeGlobs(pats []treePattern) (globs []string, of []treePattern) {
	for _, p := range pats {
		gs := []string{""}
		for i, part := range strings.Split(p.pattern, "/") {
			ways := []string{part}
			if part == "*" {
				ways = []string{"*", ".[!.]*", "..?*"}
			}
			var next []string
			for _, g := range gs {
				for _, w := range ways {
					if i > 0 {
						w = g + "/" + w
					}
					next = append(next, w)
				}
			}
			gs = next
		}
		for _, g := range gs {
			globs, of = append(globs, g), append(of, p)
		}
	}
	return globs, of
}

// treesWait is how long agentTrees waits for the VM's answer, which a
// VM's git answers in seconds: past it, the check fails rather than wait
// on a VM that never answers.
const treesWait = 2 * time.Minute

// agentTree is a git worktree an agent made in the VM, as agentTreesState
// gives it: the agent's name, its folder relative to the VM's home
// folder, its git status, the commit its HEAD is detached at when
// nothing else the VM has has it (head), and whether its git failed
// (failed), as when this repo pruned the worktree.
type agentTree struct {
	agent, path string
	status      []byte
	head        string
	failed      bool
}

// agentTreesState is the script agentTrees runs in the VM, in its home
// folder: how many worktrees to look at (cloneWorktreesMax), then each
// glob of treeGlobs, which the shell expands, as the import's clean-up
// does. For each folder a glob finds with a .git in it, it
// prints records as cloneState does, "KIND INDEX LENGTH" and that many
// bytes, INDEX the glob's: path (relative to the home folder), whead
// (the commit its HEAD is detached at, when no branch, tag or
// remote-tracking branch the VM has has it), then wstatus or wfail.
// Past the most, it prints more and stops. The VM can hide anything from
// what it's asked, so the host checks what it says, not that it says
// everything.
const agentTreesState = `cd ~ || exit 1
lim=$1
shift
t=$(mktemp -d) || exit 1
trap 'rm -rf -- "$t"' EXIT
put() { printf '%s %s %s\n' "$1" "$i" "$(wc -c < "$t/o" | tr -d ' ')" && cat -- "$t/o"; }
IFS=
i=0 k=0
for p; do
  for w in $p; do
    [ -d "$w" ] && [ -e "$w/.git" ] || continue
    k=$((k+1))
    if [ "$k" -gt "$lim" ]; then : > "$t/o"; put more; exit 0; fi
    printf '%s' "$w" > "$t/o"; put path
    if ! git -C "$w" symbolic-ref -q HEAD >/dev/null 2>&1 &&
      [ -n "$(git -C "$w" rev-list -n1 HEAD --not --branches --tags --remotes 2>/dev/null)" ]; then
      git -C "$w" rev-parse -q --verify HEAD > "$t/o" && put whead
    fi
    if git -C "$w" status --porcelain > "$t/o" 2>/dev/null; then put wstatus; else : > "$t/o"; put wfail; fi
  done
  i=$((i+1))
done`

// agentTrees reads the agents' worktrees that pats find in t's running
// VM (agentTreesState), in one command, at most cloneStateMax bytes, past
// which the command is cancelled, and for at most treesWait, and
// shows each one's work that would be lost, at most cloneShown changes in
// all: its uncommitted changes, the commits of its detached HEAD that
// nothing else in the VM has, and a worktree whose git failed, or more
// worktrees than devvm checks, which may hold either. It reports whether
// there's any. Failing, or an answer devvm can't take, is its failure
// (trees_check_failed), as the VM may hold work devvm can't see.
func (e workCheck) agentTrees(ctx context.Context, t *target, pats []treePattern) (bool, error) {
	if len(pats) == 0 {
		return false, nil
	}
	vm := t.f.Name
	globs, of := treeGlobs(pats)
	argv := append([]string{"sh", "-c", agentTreesState, "sh", strconv.Itoa(cloneWorktreesMax)}, globs...)
	run, cancel := context.WithTimeout(ctx, treesWait)
	defer cancel()
	out := &bounded{max: cloneStateMax, cancel: cancel}
	err := t.b.Exec(run, vm, argv, backend.ExecOpts{Stdout: out, Stderr: e.vmLog()})
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	var trees []agentTree
	more := false
	if err == nil && !out.over {
		trees, more, err = parseAgentTrees(out.Bytes(), of)
	}
	if err != nil || out.over {
		return false, msg.TreesCheckFailed(e.command, vm)
	}
	return e.showTrees(trees, more), nil
}

// showTrees shows, for agentTrees, each of trees with work it would lose
// under its heading, at most cloneShown changes in all, and more, and
// reports whether there's any.
func (e workCheck) showTrees(trees []agentTree, more bool) bool {
	shown, lost := 0, more
	for _, w := range trees {
		at := "~/" + w.path
		switch {
		case w.failed:
			e.Say(msg.AgentTreeUnread(w.agent, at))
			lost = true
		case len(bytes.TrimSpace(w.status)) > 0:
			lost = true
			if shown < cloneShown {
				e.Say(msg.AgentTreeDirty(w.agent, at))
				shown += e.showChanges(w.status, cloneShown-shown)
			}
		}
		if w.head != "" {
			e.Say(msg.AgentTreeDetached(w.agent, at))
			lost = true
		}
	}
	if more {
		e.Say(msg.AgentTreesUnchecked(strconv.Itoa(cloneWorktreesMax)))
	}
	return lost
}

// errTreesState is parseAgentTrees' error: the VM's answer isn't what
// agentTreesState prints.
var errTreesState = errors.New("the agents' worktrees' answer isn't agentTreesState's")

// parseAgentTrees reads agentTreesState's records for pats. The VM's
// answer is untrusted: each record must be whole and of a pattern asked
// for, each path a clean relative path that its pattern matches, each
// once and at most cloneWorktreesMax, followed by one status, and more
// comes last.
func parseAgentTrees(out []byte, pats []treePattern) ([]agentTree, bool, error) {
	var trees []agentTree
	seen := map[string]bool{}
	var cur *agentTree
	at, more := 0, false
	r := bufio.NewReader(bytes.NewReader(out))
	for {
		kind, i, body, err := nextRecord(r, len(pats))
		if errors.Is(err, errNoRecord) {
			break
		}
		// Nothing follows more, and a worktree's records are all its
		// pattern's.
		if err != nil || more || cur != nil && (i != at || kind == "path" || kind == "more") {
			return nil, false, errTreesState
		}
		switch kind {
		case "path":
			p := string(body)
			ok, _ := path.Match(pats[i].pattern, p)
			if !ok || path.Clean(p) != p || seen[p] || len(trees) == cloneWorktreesMax {
				return nil, false, errTreesState
			}
			seen[p], at = true, i
			cur = &agentTree{agent: pats[i].agent, path: p}
		case "whead":
			sha := string(bytes.TrimSuffix(body, []byte("\n")))
			if cur == nil || cur.head != "" || !objectName.MatchString(sha) {
				return nil, false, errTreesState
			}
			cur.head = sha
		case "wstatus", "wfail":
			if cur == nil {
				return nil, false, errTreesState
			}
			cur.status, cur.failed = body, kind == "wfail"
			trees, cur = append(trees, *cur), nil
		case "more":
			more = true
		default:
			return nil, false, errTreesState
		}
	}
	if cur != nil {
		return nil, false, errTreesState
	}
	return trees, more, nil
}
