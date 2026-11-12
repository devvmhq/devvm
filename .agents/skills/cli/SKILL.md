---
name: cli
description: devvm's command line conventions, as a checklist for adding or changing a command, its arguments, flags, help, prompts, --json or usage errors, with the tests that check each rule and the rules a reviewer checks by hand. Use before writing a new command, such as links between VMs' devvm link and devvm group, when changing one, and when reviewing a change to internal/cli.
---

# devvm's CLI conventions

devvm's command line should be easy to learn: once you know a few commands, you can guess the rest. This skill holds the rules that make it so, as they apply in devvm, so a new command fits the tree beside the others. Follow [the checklist](#adding-a-command) when you add a command, and [Reviewing a command](#reviewing-a-command) when you check one.

The reasoning behind most rules is in the [Command Line Interface Guidelines](https://clig.dev/), and the mechanics of commands, flags and groups are in [Cobra's docs](https://cobra.dev/docs/). Read them there, because this skill doesn't restate them. It says only what devvm decided and how devvm's code does it.

[judgment.md](judgment.md) explains, with examples, the rules that no test can check: where a command goes, rm or unX, which commands are settings, what an argument is, what a destructive command does, and how help and usage errors are worded. [Contributing](../../../docs/content/docs/contributing/_index.md) covers the code: [Command arguments](../../../docs/content/docs/contributing/cli.md#command-arguments) for the checks in `internal/cli/args.go`, [JSON output](../../../docs/content/docs/contributing/cli.md#json-output) for `--json`, [A command's help](../../../docs/content/docs/contributing/docs.md#a-commands-help) for help and groups, and [The CLI rule tests](../../../docs/content/docs/contributing/cli.md#the-cli-rule-tests) for what each test checks.

## The tree

devvm has 35 commands at the top, plus help. Each one is in a group of the root's help, in this order. The table shows each command with its subcommands and the values a setting takes, and each command's help lists its flags.

| Group | Commands |
| --- | --- |
| Your VM | devvm, with --tools, --agents and --layers; restart, stop, update, rm [VM], adopt [VM], ls, code, desktop, host, history, history export [FILE], history import [FILE], paste, paste rm |
| Change your VM | tools (a picker in a terminal), tools add\|rm TOOL..., tools update; size, with --cpus, --memory and --disk; mount-cache [none]; ports [all\|none\|LIST], ports add\|rm LIST; network [open\|internet\|learn\|allowlist\|none], network allow\|deny\|rm RULE..., network log, network lock, network transparent [on\|off]; ssh-agent [on\|devvm\|off], ssh-agent add\|rm KEY_FILE; git-protect [on\|off]; clone [on\|off], clone fetch, clone add PATH; secret, secret set\|rm\|use\|unuse NAME; layer add\|rm LAYER... |
| Links between VMs | group, group join NAME, with --as NAME, group leave; link, link add\|rm TARGET PORT, with --as NAME on add; link publish [PORTS], link unpublish PORTS |
| Images | images, images rm IMAGE\|KEY..., build IMAGE, rebuild IMAGE, layer new NAME |
| Disk space | space, gc, retention [KEY=VALUE...] |
| This machine | setup, agent, agent import [AGENT...], agent login AGENT, agent save [AGENT...], doctor, upgrade, completion SHELL, limactl [ARGS...] |

size, ports and network take `-g` for what new VMs get. Every command takes `-h`/`--help`, `--no-input` and `-v`/`--verbose`, and the root takes `--version`. `devvm --help` prints the tree as it is. `helpGroups` in `internal/cli/help.go` holds the groups, in order, and each command names its own group in its `about`. A subcommand can sit in a group other than its parent's, as layer add and layer rm do, and `devvm --help` then lists it by both names in that group.

## The rules

Each rule says what it looks like in devvm, and what checks it. A test's name means `mise run test` fails when the rule breaks, and "by hand" means a reviewer checks it, with [judgment.md](judgment.md) as the guide.

1. **A bare verb acts on this folder's VM, and anything else goes under its noun.** devvm stop stops the VM, and devvm secret set acts on a secret. A bare noun shows or lists, so no command is named `ls`, `list`, `show` or `get`, and devvm ls is the one list at the top. Only ls, build, rebuild, gc, setup, doctor, upgrade, completion and limactl stay at the top as exceptions. `TestCommandNames` checks the names, and the placement is by hand ([where a command goes](judgment.md#where-a-command-goes)).
2. **rm deletes or takes out a thing, and unX undoes the relation X made.** devvm secret rm deletes the saved secret, and devvm secret unuse stops the VM using it while the secret stays saved. `TestCommandNames` checks that an unX has a sibling X, and the choice between them is by hand ([rm or unX](judgment.md#rm-or-unx)).
3. **A setting shows bare, sets with a value, and takes `-g` for what new VMs get.** devvm ports shows, devvm ports none sets, and devvm ports -g none sets what new VMs forward. With `--json` a setting only shows, because `showsOnly` refuses a value. Each setting's CLI test checks that refusal, and which commands are settings is by hand ([settings](judgment.md#settings)).
4. **Positional arguments are only the things a command acts on, each a word of its own.** devvm tools add python claude takes two tools, and a `+` appears only in an image's name, which joins its layers, such as foo+bar. devvm itself takes no argument: a new VM's parts are flags, `--tools`, `--agents` and `--layers`, each a comma-separated list or none. Everything that changes how a command acts is a flag. `TestCommandNames` checks a `Use`'s grammar, `TestUseMatchesArgs` checks that `Args` takes what the `Use` shows, and what counts as the thing acted on is by hand ([arguments](judgment.md#arguments)).
5. **Flags are local**, and only `--help`, `--no-input` and `-v`/`--verbose` are persistent. A flag name has one meaning and one form everywhere, and the only shorthands are `-h`, `-v`, `-f`, `-g` and `-n`. Reuse the adders in `internal/cli/flags.go`, such as `addForceFlag` and `addFromFlag`, when a flag means the same. `TestPersistentFlags`, `TestFlagNames`, `TestFlagNamesHaveOneMeaning` and `TestUpFlagsAreUpsAlone` check it.
6. **Special values are words and durations have one grammar.** You write none, off, all or forever, never a blank, and `noBlanks` refuses an empty flag value or argument. A duration is a whole number and `m`, `h`, `d` or `w`, read with `state.ParseDuration` in `checkFlagValue`. `TestDurationFlags` and `TestUsageErrorsEveryCommand` check it.
7. **`--force` only skips a confirmation.** A command that deletes asks unless it has `--force`, with a question that `AnsweredBy("--force")` declares, so where devvm can't ask, the question's refusal names `--force`, and deleting more always takes a flag of its own, such as devvm gc --tool-images. `TestQuestionsDeclareAnswers` checks that `--force` is a flag devvm has, and the rest is by hand ([destructive commands](judgment.md#destructive-commands)).
8. **Every question has a flag or a command that answers it**, and where devvm can't ask (no terminal, `--no-input`, the input ended), the question is refused in one way, naming that answer. There's no blanket `--yes`. Each question declares its answer as it's made: `msg.X(...).AnsweredBy("--flag")`, or for one only the user can answer, `RefusedWith(msg.Y(...))`, the refusal that says what to do instead. The Reporter's `Ask`, `Confirm`, `Answer` and `Secret` take only such a question and return its refusal as an error, which the command returns, or which a question with a usual answer falls back on; the caller never checks for a terminal. `TestQuestionsDeclareAnswers`, `TestCheckAnswer` and `TestFlagNames` check it.
9. **Every command that shows or lists something takes `--json`.** Its output is a documented shape with `"version": 1` first, and later versions only add fields. `TestShowCommandsTakeJSON` and `TestJSONShapes` check it, and the command's docs page shows the shape with an example.
10. **Help has one shape.** A `Short` is an action in sentence case without a full stop, a `Long` has one to three sentences, `Example` comes from `examples()` with the most common invocation first, and the help ends with the command's docs page. The root's commands are in Cobra groups. `TestEveryCommandsHelp`, `TestHelpGroupsAndDocs`, `TestDocLinks` and `TestReferenceIsCurrent` check the shape, and the wording is by hand ([help](judgment.md#help)).
11. **Usage errors are devvm messages, checked before anything runs.** They name the argument or the values the command expected, and `usage_help` follows each one with the command's own `--help`. `TestUsageErrorsEveryCommand` checks that each one is a usage error and nothing runs, and the wording is by hand ([usage errors](judgment.md#usage-errors)).
12. **devvm takes no word of its own.** `devvm WORD` is an unknown command, a usage error (`checkWord` in `internal/cli/word.go`), so an image's, a shared tool's or a layer's name is never a command line's word, and a layer may have a command's name. A toolchain's, an agent's or a layer's name points to its flag (`no_command_part`), and any other word gets a did-you-mean over the commands. `TestUnknownWord` checks it.
13. **Exit codes are 0 for success, 1 for a failure, 2 for a usage error and 130 for an interrupt.** `TestExecuteExitCodes` and `TestRanErrorIsAFailure` check them.

## Adding a command

Work through this list for each new command, in order, then run [the review](#reviewing-a-command) on it.

1. **Place it in the tree.** Decide which noun it goes under, or whether it's a bare verb on this folder's VM ([where a command goes](judgment.md#where-a-command-goes)). Name it with a lower-case word, `add`, `rm` or `unX` where those fit ([rm or unX](judgment.md#rm-or-unx)), and give it no aliases.
2. **Write its `Use`.** Show each argument as a `PLACEHOLDER` in capitals or a word to type, with `[]` for optional, `|` between choices and `...` for one or more, such as `add TARGET PORT`. Only the things the command acts on are arguments ([arguments](judgment.md#arguments)). A free-form argument, such as a name or a target, goes after a verb, because a bare noun with subcommands would read it as a subcommand.
3. **Check its arguments in `Args`** with `noArgs`, `exactArgs(n)`, `maxArgs(n)` or `minArgs(n)` from `internal/cli/args.go`, never Cobra's own. Set `ValidArgs` for a fixed set of words, and check values with `cobra.MatchAll`, returning the same message the command would fail with later.
4. **Add its flags** in the command's own `Flags()`, reusing an adder from `internal/cli/flags.go` or a flag name that already means the same thing. A new flag name means one thing, and its help text is lower case without a full stop, with its value as a backticked placeholder, such as ``the `N` CPUs the VM gets``. Check a flag's value in `checkFlagValue` and flags that can't go together with `exclusive`.
5. **Settings:** when the command shows a setting bare and changes it with a value, wrap its `Args` in `showsOnly`, and give it `-g` (`--global`) when new VMs have a default for it ([settings](judgment.md#settings)).
6. **Questions:** every question the command asks needs a flag or a command that answers it beforehand, declared where it's asked: `u.Ask(msg.X(...).AnsweredBy("--flag"))`, so its refusal names it. A command that deletes takes `addForceFlag`, asks only without `--force`, with `AnsweredBy("--force")`, and returns the question's error ([destructive commands](judgment.md#destructive-commands)).
7. **Output:** when it shows or lists something, give it `jsonFlag`, a struct of its own with snake_case `json` tags, `printJSON`, a row in `TestJSONShapes` and a section with an example on its docs page. Otherwise say why it has none in its `about`'s `noJSON`, such as `noJSON: "it deletes"`.
8. **Help:** write a `Short`, a `Long` of one to three sentences and an `Example` from `examples()`, with the docs-voice skill ([help](judgment.md#help)). Set its `Annotations` from an `about` (`internal/cli/help.go`): `docs`, the user page that covers it as `page#anchor`, and for a root command its `group`, one of `helpGroups`, such as `Annotations: about{docs: "using/vms#listing-vms", group: groupVM}.annotations()`.
9. **Messages:** every message goes in `locales/en.toml`, with the docs-voice skill's Messages rules, and then `go generate ./internal/msg`. A usage error names what the command expected ([usage errors](judgment.md#usage-errors)). The command fails by returning its error from `RunE`, as `failed(err, msg.XFailed)` when the error may not be a message, or through `a.env`, which does the same, and never prints the failure itself, because `execute` shows it (`TestExecuteShowsEachFailure`).
10. **Register it** with `AddCommand` in `NewRoot` (`internal/cli/root.go`) or under its noun, and write it in its own file, `internal/cli/<command>.go`, with what completes its arguments and flags (`ValidArgsFunction`, `RegisterFlagCompletionFunc`) set there too.
11. **Tests:** add a CLI test in `testdata/script/` for what it does and for each refusal, with `DEVVM_MESSAGES=names`. Add a sample value to `argSamples` in `internal/cli/args_test.go` when `TestUseMatchesArgs` asks for one, for a placeholder no command had, and a command on this folder's VM to `folderCommands` in `internal/cli/flags_test.go`.
12. **Docs:** add the command to its group's table on the Commands page (`docs/content/docs/getting-started/commands.md`), and to the page its `about` names, with the docs-voice skill. Then run `mise run cli-docs` and commit the pages it writes.
13. **The gate:** `mise run fmt && mise run lint && mise run test`.

## Changing a command

A rename, a new flag or a changed argument follows the same steps for what changed. Then search the whole repo for the old form and fix every use of it: the docs, `locales/en.toml`, Go strings and comments, `testdata/script/`, `tests/e2e/`, `install.sh`, `README.md`, `AGENTS.md`, the scripts in `assets/` and the skills. `TestCommandLines` fails on most old forms, a command, a flag or a placeholder that's gone, but it reads only a sentence's command and flags, so read the prose around each change too. An old name gets no alias, and the docs describe only the command as it is.

## Reviewing a command

Run the command's help and read the code, then check what the tests can't:

- **Placement:** it sits where a user would look for it, and it reads as a verb on this folder's VM or a noun with its verbs.
- **rm or unX:** rm makes the thing go, and unX leaves the thing and undoes only the relation.
- **Settings:** a command that holds a value shows it bare and sets it with a value, and `-g` appears only where new VMs get a default.
- **Arguments:** each positional argument is a thing the command acts on, and nothing that changes how it acts is positional.
- **Destructive commands:** it asks unless given `--force`, its question answered by `--force` and refused where devvm can't ask, and `--force` never deletes more than the command does without it. Deleting more has its own flag.
- **Help:** the `Short` names what it acts on, such as this folder's VM, the `Long` says what it does, and the rules and edge cases go on the docs page. The examples start with the common case, and each comment says what that line does.
- **Usage errors:** each one names the argument or the values the command expected, as the `Use` shows them.
- **The tree:** `devvm --help` still matches [the tree](#the-tree), and the Commands page's tables match `devvm --help`.

Then run the gate, and read the command's page in `docs/content/docs/cli/` as a user would.
