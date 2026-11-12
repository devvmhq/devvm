---
name: docs-voice
description: Rewrites devvm's prose (the docs site, READMEs, command help, CLI messages) into a plain, easy-to-follow guide, finding and fixing compressed writing and AI writing tropes without losing any fact. Use when writing or editing docs, or when asked to clean up the wording, voice or readability of a page.
---

# Docs voice

devvm's docs should read like a guide a colleague wrote: full sentences that say what devvm does and what you do, in the order you'd need them. Every fact stays, and only the wording changes.

[tropes.md](tropes.md) lists what to fix, with examples. [scan.sh](scan.sh) flags candidates, and [facts.sh](facts.sh) checks that a rewrite kept every fact.

## The voice

- **Full sentences.** Each one has a subject, a verb and its articles. Name the actor: devvm, you, the VM, the proxy.
- **Joined, not chopped.** Connect related facts with "and", "but", "so", "because", "when" or "if". Vary sentence length, and avoid runs of sentences under eight words.
- **One idea per sentence.** A sentence that needs a colon, a semicolon and a parenthetical holds two or three ideas, so split it.
- **What it does first.** Describe the behavior, then its limits and exceptions. Use negatives for guarantees.
- **The reader's order.** Lead a page or section with what the reader wants to do, then how, then the details and edge cases.
- **Exact.** Keep every flag, path, command, message, number and limit. Plain wording never means vaguer wording.
- **Present tense, current state.** Describe how devvm works now. Don't mention older behavior, what changed or what moved.
- **House rules.** Write commands as devvm x or in backticks, never in quotes. Never write "please", "note that" or "simply". Use sentence-case headings. Address the reader as "you".

## Commands first on user pages

The user pages, in Getting started, Using devvm, Staying safe, Backends and Compared (`getting-started/`, `using/`, `safety/`, `backends/` and `compared/`), are where readers go to get something done or to learn what they can rely on, so each section there that has a task starts with what to type: a copyable command block, or a numbered list of steps, with a comment on each line that says what it does. Explanation follows, and it stays short.

- **One paragraph is fine.** A paragraph that says why, next to the command it's about, is good, and so is one that gives the details a reader needs while doing the task, such as a limit, a question devvm asks or the flag that answers it.
- **The target is paragraphs back to back.** Two or more prose paragraphs with no command, list, table or callout between them are the sign that mechanism has crept in. Split them with the command each one describes, turn parallel facts into a short list, or move the mechanism to its How it works page.
- **Move mechanism, don't delete it.** How devvm does something, such as what it checks, in what order, and why an attack fails, goes to its How it works page in `internals/`, under a heading that a link from the user page names. Leave a sentence that says what the reader gets and links there.
- **Keep every fact.** A move is a cut and a paste, not a summary. Run `facts.sh --all REV` after a move, so a fact that went to another page doesn't show as removed, and add the heading on the How it works page first if it doesn't exist.
- **Lists for parallel facts only.** A list of modes, refusals, limits or what a command keeps is easier to scan than a paragraph. Facts that build on each other stay prose, as [Bold-label bullets](tropes.md#bold-label-bullets-boldlabel) says.

`scan.sh -p paras FILE` lists each run of paragraphs, two or more on a user page and three or more in `internals/` and `contributing/`, with each paragraph's words, its line and its heading. A paragraph of 40 words or fewer that ends with a colon doesn't count, because it introduces the block after it. `scan.sh -p long FILE` lists each paragraph or list item over 80 words or 4 sentences. A run or a long paragraph is a candidate, not an error: leave one that a reader needs in one piece, such as a warning and the action that follows it. `mise run lint:docs` fails on the worst of them on a user page: a run of three or more paragraphs where two are over 80 words, and a paragraph or list item over 150 words.

Before, from Troubleshooting's A command fails (five paragraphs in a row, abridged):

> devvm prints its own steps, and it sends the output of the commands it runs to a log instead of the terminal. When a step fails, devvm prints the end of the log.
>
> To see the commands' output as they run, add `-v` or `--verbose` to any command, as in `devvm -v build base` or `devvm -v`. devvm then shows each line on standard error … and devvm drops the control characters a command prints …
>
> Each run that runs a command has one log, `~/.local/share/devvm/logs/runs/<date>-<time>-<pid>.log` … A command that devvm runs in a VM adds at most 4 MiB of output to the log … A VM's proxy key and the build proxy's password are written as `devvm:<key>@` …
>
> Lima keeps a log of its own for each VM, `ha.stderr.log` … devvm replaces each password there with as many `*` …
>
> In a terminal and without `-v`, a status line shows the step, how long it has run, and the log's latest line …

After:

> devvm prints its own steps and sends the output of the commands it runs to a log, and when a step fails, it prints the end of that log. To watch the output as it runs, add `-v`:
>
> ```sh
> devvm -v              # open this folder's VM, showing each command's output
> devvm -v build base   # the same for any command
> ```
>
> With `-v`, devvm shows each line on standard error as the log gets it, in place of the status line, and when a step fails it names the log instead of printing its end again. Without `-v`, a status line in a terminal shows the step, how long it has run and the log's latest line, and a step that takes more than a few seconds leaves a line with its time.
>
> - **The log** is `~/.local/share/devvm/logs/runs/<date>-<time>-<pid>.log`, one for each run, which only you can read, and devvm keeps the newest 20.
> - **Each command** appears in it as a `+` line with its arguments, followed by its output, and a numbered step adds a `# [2/5] <step>` line first.
> - **Passwords and a VM's proxy key** stay out of it, written as `user:<password>@` and `devvm:<key>@`, including a company proxy's password from your shell's `HTTPS_PROXY`, and How it works would have the rest.

The rest would move to a new The run log section on How it works' Overview, which the after version links to: the 4 MiB cap on a VM command's output and the `devvm: truncated` line, the build proxy's password, a proxy address without a scheme, passwords written in pieces, the VM's setup and a `devcontainer.json`'s commands being covered, `-v` showing the log's own lines with control characters dropped, and Lima's `ha.stderr.log`, where devvm replaces each password with as many `*`, warns when it can't and lets you delete the file.

## Scope

All prose in the repo:

- `docs/content/**/*.md`, except `docs/content/docs/cli/`, which is generated.
- The command help that generates those CLI pages: `Short` and `Long` in `internal/cli/*.go`. Run `mise run cli-docs` after editing them.
- `README.md`, `SECURITY.md`, `CONTRIBUTING.md`, `assets/oci/README.md` and `internal/proxy/README.md`.
- The messages in `locales/en.toml`, which also follow the [Messages](#messages) rules below.
- The cli skill, `.agents/skills/cli/`.

These follow their own rules and are out of scope:

- `AGENTS.md`, which stays short.
- `.agents/skills/security-audit/`, which is vendored.

## Messages

Messages use the same voice as the docs, only shorter, so the CLI reads like the docs. Each one is a table in `locales/en.toml`, and its `other` and `one` strings are what devvm prints.

- **Full sentences.** Each sentence has a subject and a verb, starts with a capital letter and ends with a full stop. devvm prints errors and warnings after "devvm: ", so they read as "devvm: The VM has…".
- **A warning or an error never starts with devvm,** because devvm prints "devvm: " before it. Lead with what failed or what it's about instead, such as "Adding the git remote devvm failed." Messages printed as information, without the prefix, may start with devvm.
- **What happened first.** Lead with what happened, or with what you're asking, in the user's terms. Then give one action as a command, such as "run devvm rm" or "check its hooks".
- **One idea per sentence.** Don't chain clauses with colons and semicolons.
- **Short.** An error or a warning has at most two sentences. A prompt has at most two lines of context before its question, and it marks the answer devvm recommends with "(recommended)" when there is one.
- **The mechanism goes in the doc link.** The message says what happened and what to do, and the page it links to explains how devvm works. Every warning or error the user can act on has an entry in `internal/msg/docs.go` that points at the user page telling them what to do. Only internal failures link to a How it works page in `internals/`.
- **"WARNING:" only when the user has to act.**
- **The same terms as the docs**, from [Terms](#terms).
- **Status steps stay as they are:** a few words, lowercase, such as "building image base". Data output isn't a message.
- **House rules.** `[y/N]` and `[Y/n]` stay literal. Commands are written as devvm x, not quoted. Never write "please", "note that" or "simply". A value renders on one line without control characters (`msg.OneLine`), so a message's template never relies on a value's newlines.
- **Templates keep their fields,** so no call site changes. If a message can't be accurate without changing a field, change the field and its callers. Long `{{.vm}}` names stay where the template has them.
- **Both plural forms.** When a message has `one` and `other`, rewrite both.

[tropes.md](tropes.md#messages) has before and after examples.

## Terms

Use these terms on every page and in every message.

- **Secret** is the general term for what devvm keeps on this machine for VMs to use, as in devvm secret. The Staying safe and Network policy pages define it: "A secret is a credential devvm keeps on this machine for VMs to use: your GitHub token, Claude Code's token, an agent's login or an API key."
- **Name the secret when the specific one matters:** "GitHub token", "Claude Code token" (from claude setup-token), "API key", "Codex login", "opencode login" or "pi's API key". Never write a bare "token" or "tokens", because agents use "tokens" for model tokens. The one exception is right after a prompt that names the token: after "GitHub token:", "You didn't paste a token" is clear.
- **Feature** on user pages means only base image features (omb, fzf, cli-tools) and devcontainer features. Backend features, which are native, emulated or none, appear only on the How it works pages. devvm doctor prints backend features as "features", so say so where a user page compares backends.
- **Image** means devvm's images, such as base or foo+bar. Say "container image" for the images that `--from` and a `devcontainer.json` start from.
- **Layer** means devvm's layers. Say "Docker's image layers" when you mean Docker's.
- **Shared tool** means a toolchain or a coding agent that devvm shares read-only between VMs, such as node or claude: the toolchains page and the coding agents page's Supported agents table list them.

## Workflow

Work on one page at a time.

1. **Scan.** `.agents/skills/docs-voice/scan.sh FILE` shows the page's counts, and `scan.sh -v FILE` (or `-p NAME`) shows each hit. Run it on every in-scope file to choose the densest page first.
2. **Read the whole page** before changing it, together with the pages it links to and the pages that link to its headings (`grep -rn 'page.md#' docs/content internal/msg/docs.go`).
3. **Rewrite** paragraph by paragraph with the Edit tool, following the voice and [tropes.md](tropes.md). Restructure a section when its order is the problem, but keep its headings: other pages, `internal/msg/docs.go` and messages link to them. Rename a heading only when you update every link to it.
4. **Check the facts.** `.agents/skills/docs-voice/facts.sh FILE` lists the code spans, links, headings and numbers that changed. Each change must be one you meant. When a change moves text between pages, run `facts.sh --all REV` instead, which compares the whole docs tree at `REV` with your working tree, so a fact that moved to another page doesn't show as removed. It counts each fact's copies and ignores headings and link targets, because pages and their anchors move. Then reread the old and new versions side by side (`git diff --word-diff FILE`) for meaning: conditions, who does what, "only" and "never".
5. **Rescan.** The counts should drop. Hits that remain are allowed by [tropes.md](tropes.md)'s "Not tropes".
6. **Gate.** Run `mise run fmt && mise run lint && mise run test`. Vale's `devvm.AIWords` rule fails on chatbot vocabulary, and `TestDocLinks` fails on a broken message link.

### For messages

Work on one group of messages at a time, such as the network policy messages.

1. **Scan.** `scan.sh locales/en.toml` reads only the messages, with their `{{.fields}}` as code spans, and adds the `lower` and `end` patterns, which flag a message that doesn't start with a capital letter or end with a full stop. `-m REGEX` scans only the messages whose ID matches, such as `-m '^net_'`, and `-v` shows each hit's message ID.
2. **Read each message with its call site.** Grep the generated function's name in `internal/` to learn when the message shows and what its fields hold, and render it with `go run ./cmd/devvm _msg NAME key=value…`.
3. **Rewrite** with the Edit tool, run `go generate ./internal/msg`, and render the message again.
4. **Check the facts.** `facts.sh locales/en.toml` lists each message's fields, devvm commands, code-like words (flags, paths, file names, `NAME=value`, environment variables) and numbers that changed, each after its message's ID, so you can grep it for your group. Tests check message names and fields, not wording, so a test that breaks points at a field or a name you changed by mistake.
5. **Rescan and gate,** as for a page.
