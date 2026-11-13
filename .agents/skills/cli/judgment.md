# The rules a reviewer checks

The tests in [The CLI rule tests](../../../docs/content/docs/contributing/cli.md#the-cli-rule-tests) check a command's shape: its names, its flags, its help's parts and its `--json`. They can't tell whether a command is in the right place, whether its arguments are the right ones, or whether its words help the user. Those choices are yours, and this page explains each one with examples from devvm's own commands. Each section ends with the question to ask in a review.

## Where a command goes

A command is either a verb on this folder's VM or a noun with its verbs.

- **A bare verb acts on this folder's VM.** devvm restart, devvm stop and devvm rm need no argument, because the VM is the folder's. devvm code, devvm cursor, devvm desktop and devvm adopt are verbs of the same kind.
- **Anything else goes under its noun.** A secret is a thing of its own, so saving one is devvm secret set, not devvm save-secret. A rule belongs to the network policy, so adding one is devvm network allow, not devvm allow. A key belongs to devvm's own SSH agent, so loading one is devvm ssh-agent add.
- **A bare noun shows or lists.** devvm secret lists the secrets, devvm images lists the images, and devvm history lists the exports, so none of them has an `ls` or `show` subcommand. devvm ls is the one list at the top, because it lists every VM rather than something of this folder's VM.
- **The exceptions stay at the top:** ls, build, rebuild, gc, setup, doctor, upgrade, completion and limactl. Each is a verb a user reaches for by name, and moving build under images, as devvm images build, would make the most common image command the hardest to find. upgrade acts on devvm itself, not on this folder's VM, and it's the name other tools give the same verb. Add to that list only when the same is true, and say why in the commit.

Links between VMs follow the rule. A link is a thing of this folder's VM, so it's a noun: devvm link lists the VM's links, devvm link add TARGET PORT makes one, and devvm link rm TARGET PORT removes it. devvm group is a noun too, with devvm group join NAME and devvm group leave.

An editor devvm opens the VM in is a bare verb named after its command, as devvm code is VS Code's `code` and devvm cursor is Cursor's `cursor`. A name an editor shares with an agent is the editor's at the top, and the agent is reached only through `--agents`, as in `devvm --agents cursor`. The agent's own commands in the VM are `cursor-agent` and `agent`, so the two never meet on a command line.

When a command could go in two places, put it where a user who knows the rest of the tree would look first. A user who wants to stop forwarding a port types devvm ports, because the ports are what change, and finds devvm ports rm there.

**In a review:** would a user who knows devvm ports and devvm network guess this command's name?

## rm or unX

rm and unX both undo something, and they're for different things.

- **rm makes a thing go.** devvm rm deletes the VM, devvm images rm deletes images, and devvm secret rm deletes a saved secret. rm also takes an entry out of a list that `add` or another verb put it in: devvm tools rm takes shared tools out of the VM, devvm ports rm stops forwarding ports, devvm network rm removes rules that allow or deny added, and devvm ssh-agent rm takes a key out of devvm's agent.
- **unX undoes the relation X made, and the thing stays.** devvm secret use github lets the VM use the GitHub token, and devvm secret unuse github stops it, while the token stays saved for other VMs. devvm link publish offers a port to the user's other VMs, and devvm link unpublish withdraws the offer, while the port's server keeps running in the VM.
- **add pairs with rm, and any other verb pairs with its unX.** A list that grows with `add` shrinks with `rm`. A verb such as `use` or `publish` names a relation, so its undo is `unuse` or `unpublish`, and `TestCommandNames` checks that the verb is beside it.

Don't name a command `delete`, `remove` or `disable`, because rm and unX cover them. When you can't decide, ask what's left after the command runs. If the thing is gone, or out of the list it was in, it's rm. If the thing is still there and only its tie to the VM is gone, it's unX.

**In a review:** after this command, is the thing gone, or only its tie to something else?

## Settings

A setting is a value devvm keeps, which a user reads and changes. Each setting is one command that does both:

- **Bare, it shows the value.** devvm size shows the VM's CPUs, memory and disk, devvm ports shows what it forwards, and devvm git-protect shows its `.git` protection.
- **With a value, it changes it.** devvm ports none, devvm network open, devvm ssh-agent devvm and devvm git-protect off each set the value. devvm size takes its values as `--cpus`, `--memory` and `--disk`, because it holds three, and devvm retention takes `KEY=VALUE` words, because it holds many.
- **With `-g`, it shows or changes what new VMs get.** devvm size -g --memory 16, devvm ports -g none and devvm network -g none change the defaults. A setting takes `-g` only when new VMs get a default for it, so devvm git-protect, devvm ssh-agent and devvm clone have none, and devvm retention is about the whole machine.
- **With `--json`, it only shows.** `showsOnly` refuses a value, so devvm network --json off is a usage error and changes nothing.

The commands that are settings are size, mount-cache, ports, network, network transparent, ssh-agent, git-protect, clone and retention. devvm mount-cache holds how a Linux host's VM caches its project folder, and takes one value, none, the only mode devvm sets, as a word rather than a bare opt-in command, so it reads as what it shows and stays the same should another mode be worth setting. It has no `-g`, because every new VM gets none. devvm network transparent has no `-g` either: a VM without its own value follows config.toml's `transparent-proxy` as it is at each launch, so there's no default new VMs take. devvm clone holds whether the VM has its own clone of the repo, the setting `--clone` gives a new VM, and setting it makes the VM again, as devvm layer add does. tools isn't one, because its value is a list that devvm tools add and devvm tools rm change, and that devvm tools shows in a terminal as a picker whose changes become add and rm. setup isn't one either: it asks the first-run questions, and its flags, `--features` and `--agents`, answer the two that no other command sets.

A command is a setting when its bare form shows one value and the user can type the new value after its name. When the value is a list that grows and shrinks, give it add and rm, as devvm ports has besides devvm ports LIST.

**In a review:** does the bare command show the value, does a value set it, and does `-g` appear only where new VMs have a default?

## Arguments

A positional argument is a thing the command acts on, and everything else is a flag.

- **Things acted on are arguments.** devvm build foo+bar builds that image, devvm secret rm github deletes that secret, and devvm link add db 5432 links to that VM's port.
- **How a command acts is a flag.** devvm link add db 5432 --as postgres names the link postgres, which changes how the link is made but isn't what's linked. devvm gc --history deletes more kinds of things, and devvm network log --blocked shows one kind of entry. devvm acts on this folder's VM, so what a new VM gets is how it makes the VM: devvm --agents claude --layers docker, never a word after devvm.
- **Each thing is a word of its own.** devvm tools add python claude takes two tools, never joined with a comma or a `+`, and devvm images rm IMAGE|KEY... takes as many images as you give it. A `+` only ever joins an image's layers, as in foo+bar.
- **One value is one word, even when it's a list.** devvm ports 5173,8000-8010 sets one value, the ports to forward, so its list is one word with commas, as `--features omb,fzf` is.
- **Fixed positions read in the order a user says them.** devvm link add TARGET PORT names what it links to, then which of its ports.
- **A free-form argument goes after a verb.** devvm secret set NAME and devvm link add TARGET PORT put the name after a subcommand, so a secret or a VM called `add` can't be read as a command. A bare noun's own argument is only a fixed word or a value no subcommand can look like, such as devvm network internet or devvm ports 5173.

Write the arguments in the `Use` as a user types them: `PLACEHOLDER`s in capitals, words in lower case, `[]` for an optional one, `|` between choices and `...` for one or more. A `Use` shows arguments only, so `gc [--tool-images [TOOL...]]` shows its flags only because `TOOL...` needs `--tool-images`.

**In a review:** is each argument a thing the command acts on, and would moving any of them to a flag make the command clearer?

## Destructive commands

A command that deletes something the user can't get back asks first, and a script must say so.

- **It asks unless `--force`.** devvm gc, devvm gc --caches, devvm images rm and devvm secret rm in `internal/cli`, and devvm rm and devvm rebuild in `internal/vm` and `internal/build`, go ahead at once with `--force`, and otherwise ask with `Ask` or `Confirm` and return the question's error. Where devvm can't ask, without a terminal, with `--no-input` or once the input ends, that error is the question's refusal, which shows the question and names `--force`. That refusal is a failure, exit 1, not a usage error.
- **It takes `--force` from `addForceFlag`, which only skips the question.** devvm gc --force deletes exactly what devvm gc lists and asks about. `--force` never deletes more, never skips a safety check that isn't the question, and never answers another question.
- **Deleting more takes a flag of its own.** devvm gc deletes stale images and unused tool images, and each wider kind has its own flag: `--tool-images` for the current shared tool images no VM has, `--history` for agents' history exports, `--orphans` for what devvm keeps for VMs that their backend no longer has, `--nix-cache` for the shared Nix cache and `--caches` for Lima's own caches. A user reads what a command deletes from its flags.
- **Skipping a step takes a flag of its own too.** devvm rm exports the agents' history first, and `--no-export` skips the export, so devvm rm --force still exports.
- **Its question is answered by `--force`,** as `AnsweredBy("--force")` declares, so the refusal names `--force`. A question that protects work only the user can judge, such as devvm rm deleting the VM of a folder this one is inside, has no flag, and declares its refusal with `RefusedWith`, a message saying what to run instead.

A command that takes out only what the user named, and what one command puts back, asks nothing: devvm network rm, devvm ports rm and devvm tools rm run at once. devvm link rm TARGET PORT is of that kind, because devvm link add TARGET PORT makes the link again.

**In a review:** can a user lose work through this command without a question in a terminal or `--force` in a script, and does `--force` do anything but skip the question?

## Help

A command's help is short, and its docs page holds the rest. Write it with the docs-voice skill.

- **`Short` is an action on a named thing.** "Delete this folder's VM", "Forward more ports from this folder's VM" and "Load a key into devvm's own SSH agent" each say what happens and to what. A noun's `Short` says what it shows: "List the secrets kept on this machine, for VMs to use". A settings command starts with "Show or change", as in "Show or change this folder's VM's size".
- **`Long` says what the command does in one to three sentences.** devvm rm's says what it deletes, that it asks in a terminal, and that it exports the agents' history unless `--no-export` is given. Edge cases, limits and how it works go on the docs page that its `about` names.
- **The examples start with the common case.** devvm rm's first example is devvm rm, then devvm rm --force, then devvm rm --no-export. Each comment says what that line does, in lower case, such as "delete it without asking, as a script must". A noun's examples can show its subcommands, as devvm secret's show devvm secret set github.
- **A flag's help is a short phrase in lower case without a full stop,** with its value as a backticked placeholder, such as ``the `N` CPUs the VM gets``. A default goes at its end in parentheses, as in "(default: devvm ports -g, else all)".
- **The same thing has the same words everywhere.** Say this folder's VM, new VMs, devvm's own SSH agent and shared tools, as the docs-voice skill's Terms say. A flag shared between commands has one help text, unless `manyUses` in `internal/cli/flags_test.go` says why it differs.

**In a review:** can a user who reads only the `Short` and the first example run the command correctly?

## Usage errors

A usage error tells the user what the command expected, before anything runs, so they can fix the line and run it again.

- **Name what was expected, as the `Use` shows it.** "KEY_FILE is missing from devvm ssh-agent add." names the placeholder from the help, so the user can find it there. "There's no command devvm secret nope. The commands under devvm secret are rm, set, unuse, use." lists the choices.
- **Use the command's own message for a bad value.** devvm ports maybe fails with the port list's message, which says what a port list is, and `Args` returns it through `cobra.MatchAll` before the command looks for a VM.
- **Don't repeat what follows.** `Execute` prints `usage_help` after every usage error, which names the command's own `--help`, and the message's doc link goes to the page that explains it. A usage error's own text says only what was wrong and what was expected.
- **A usage error is about the command line.** A missing VM, a stopped VM or a refusal without a terminal is a failure, exit 1, with a message that says what to run.

Write usage errors with the docs-voice skill's Messages rules: at most two sentences, each a full sentence, with the command written as devvm x.

**In a review:** does the error name the argument, the value or the choices the user should have typed?
