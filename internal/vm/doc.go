// Package vm runs project VMs: what each of devvm's commands does to a
// folder's VM, below the command line.
//
// Env (env.go) is what they run with, the shared dependencies, and its
// parts, each made once by wire, which the commands reach without ever
// converting one type to another. The rest, by concern:
//
//   - The start: start.go's starter.bringUp is every path's one start,
//     with its requirements, confinementFor and hostChecks.
//   - The finder: finder.go's target finds the VM a command acts on, as
//     its need says; target.go is that VM, read once, and what a command
//     does with it; move.go is a VM whose folder moved, devvm adopt VM.
//   - The inventory: inventory.go reads every VM's state, backend and
//     list in one place, with one failure policy.
//   - Launch opens a shell in a folder's VM: a new one (up.go; parts.go,
//     its toolchains, agents and layers, the picker, and setup's agents
//     and layers; detect.go, what the folder's files call for), an
//     existing one (existing.go), and ssh.go.
//   - The agents: their mounts from devvm's agent store (agents.go's
//     agentStore, the one reader of Dirs.AgentSettings, while
//     secrets.LoginFile reads the logins); AgentImport (agentimport.go,
//     devvm agent import, the one reader of the home's agent files);
//     AgentLogin (agentlogin.go, devvm agent login: the agent's login in
//     the VM, its forward, API keys); AgentSave (agentsave.go, devvm agent
//     save: a VM's settings into the store after a diff, linediff.go, and
//     a yes; agentsave_read.go, its fixed list and the checks on the VM's
//     stream).
//   - Lifecycle (lifecycle.go) stops, adopts, deletes and exports a VM;
//     remake.go is the one remake, a VM made again from another image, for
//     devvm layer add, devvm layer rm, devvm update and devvm clone, and
//     home.go its home folder's copy.
//   - Housekeeping (housekeep.go): images, shared tools, exports and
//     leftovers, for devvm images, gc, space and retention; Exports
//     (exports.go): the exports of agents' history, their .folder files,
//     Newest.
//   - Tools (tools.go): a VM's shared tools; Settings (settings.go,
//     size.go, ports.go, sshagent.go, transparent.go, mountcache.go): its
//     size, ports, SSH agent forwarding, .git protection, transparent mode
//     and its project folder's cache mode; Net
//     (network.go, secret.go): its network tier, network log, allowlist
//     and secrets.
//   - Links (links.go): links between VMs and published ports; Groups
//     (groups.go): the links a group makes.
//   - --clone (clone.go): the clone, rm's check (clonecheck.go: every
//     repo read in one command) and devvm clone's switch; a workspace
//     clone of a folder of repos (workspace.go: finding and choosing
//     them, their seeds, devvm clone fetch and devvm clone add;
//     workspacefiles.go: the folder's own files, copied once).
//   - devcontainer.go: a devcontainer.json's mapping and commands; sync.go:
//     the setup devvm sends to a VM.
//   - Paste (paste.go): devvm paste, an image, a PDF or a copied file from this
//     machine's clipboard into the VM's paste folder, devvm paste rm, and
//     the launch's offer to delete what's left there.
package vm
