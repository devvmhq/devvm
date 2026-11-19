---
title: The CLI and its messages
explainer: Commands, arguments, messages and JSON output
weight: 5
---

## CLI conventions

devvm's commands follow one set of rules, so that a user who knows a few of them can guess the rest. A bare verb acts on this folder's VM, anything else goes under its noun, and a bare noun shows or lists. A setting shows bare, changes with a value and takes `-g` for what new VMs get. The [cli skill](https://github.com/devvmhq/devvm/blob/main/.agents/skills/cli/SKILL.md) holds every rule, with a checklist for adding a command and a review for changing one, and its [judgment.md](https://github.com/devvmhq/devvm/blob/main/.agents/skills/cli/judgment.md) explains, with examples, the rules that only a reviewer can check. The reasoning behind most of them is in the [Command Line Interface Guidelines](https://clig.dev/), and Cobra's own mechanics are in [Cobra's docs](https://cobra.dev/docs/).

The tests check every rule that a machine can, and `mise run test` runs them. The cli skill says which test checks each rule and which rules a reviewer checks by hand, and [The CLI rule tests](#the-cli-rule-tests) says what each test checks. After changing a command's `Use`, `Short`, `Long`, `Example` or flags, run `mise run cli-docs` and commit the [CLI reference](docs.md#the-cli-reference) it writes.

Each command gets the run's services from one `app`, in `internal/cli/app.go`, which `NewRoot` makes for the run's UI and hands to every command's constructor. The app holds devvm's folders (`dirs`), a test build's overrides, the secrets store, the backends' registry, the shared tools, the environment of the commands on this folder's VM (`vmEnv`) and the image builder of the backend new VMs get (`builder`, the VM environment's `Builder`). It makes each one the first time a command asks for it and keeps that one, or its error, for the rest of the run, so the retention that runs before a command and the command itself share them. A command on this folder's VM runs in `a.env`, which returns the command's failure with its message for an error that isn't one. A new command takes its services from the app rather than making its own.

A command fails by returning its error from `RunE`, as `failed(err, what)` when the error may not be a message: `what` makes the message from the error's text, such as `msg.LsFailed`. `execute` shows what the command returned in `showFailure`, and it's the one place a failure is printed. A `backend.LoggedError` shows the end of the run log, then its message (`ErrorLogged`). A message, or an error that carries one, shows as it is, and any other error shows in `what`'s message, or in `output_failed` when the command didn't say what failed. The run then exits with 1, and `ui.Main` runs the cleanups. Once the run is interrupted, a failure prints nothing, because the interrupt is the reason. The root's layout check (`checkLayout`) returns its failure the same way, from `PersistentPreRunE`. Only the UI ends a run itself, in `die`, where no command can return: a question that gets no answer, and a run log that can't be opened.

INT, TERM and HUP cancel the run's context in `ui.Main`, which then waits for the logged command that's running to end. When there are cleanups to run, it also waits for the command's own code to return, for up to 2 seconds (`returnGrace`), before it runs them and exits with 130. So a cleanup never undoes a command's work while that command still runs, and a command that can't stop delays the exit by no more than those 2 seconds. A question waiting for its answer isn't waited for at all, because reading its answer doesn't end at Ctrl+C, and an answer read once the run is interrupted is refused, so nothing goes ahead after it. A run with no cleanups exits at once, because nothing could undo its work, so the proxy and devvm's other background processes stop as soon as they're told to. A second signal during the cleanups is ignored, so they aren't stopped halfway.

Ctrl+C reaches every process in the terminal's group, so it can end a command that devvm runs before devvm itself sees the interrupt. When a command ends by a signal, or exits with a code of 128 or more, devvm waits up to 1 second for its own interrupt (`interruptGrace`), so that the run ends as interrupted, printing nothing, instead of showing the command's failure.

Whatever a failure would leave behind, such as a build VM or a temporary file, registers a cleanup with the UI's `AddCleanup` and removes it, with the function `AddCleanup` returns, once there's nothing left to clean up. The cleanups run when the run ends, last added first, whether it ended normally, failed or was interrupted. They run after the command returned and the run's context is canceled, so each one gets a fresh context of its own. A cleanup that only undoes unfinished work after an interrupt checks `Interrupted()`, and the command leaves it registered once the run is interrupted, because the cleanups run only after the command returns.

devvm runs every external command through the UI's `LoggedCmd(ctx, step, cmd)`, which a backend gets as its `Runner`. The command's output goes to the run log, a terminal's status line shows `step`, and a failure after it is returned as `backend.Logged(m)`, which shows the end of the log with the message. `WithSteps(ctx, n)` starts a phase of `n` steps, and each `LoggedCmd` with the context it returns is numbered, as `[2/5] running base.sh`, on its status line and in the log.

### Status lines

Any work that can take noticeable time runs under a status line, so that the user sees what devvm is waiting for. That's a host command, host git, a command in a VM, a backend's own commands, such as `limactl list`, a request over the network, a walk of a folder the user owns, a lookup of a folder's disk with macOS's `diskutil`, and a wait for a lock, a timeout or another process. A logged command shows its step on its own, and other slow work shows one with the Reporter's `Status`, whose label is a message in `locales/en.toml`, a lowercase phrase in `-ing`, such as `reading_vms`, with a `description` that says what runs under it:

```go
func ListAll(ctx context.Context, e *Env) ([]Listed, []Known, error) {
    defer e.Status(msg.ReadingVMs())()
    inv, err := ReadInventory(ctx, e.Dirs, e.Registry, AllBackends)
```

The status line shows only in a terminal, without `-v` and until the run is interrupted, as nothing would clear one started after that. The UI keeps status lines as a stack: a logged command or a `Status` inside another shows its own line until it ends, and then the outer one is drawn again, so a phase can show one line around steps that show their own. A message said under a status line, whether `Info`, `Warn`, `Note` or data on `Stdout()`, clears the line, prints, and draws the line again below it, so a warning never lands on the status line. A question, the picker and `ui.Terminal`, which a command in the VM that draws on the terminal gets, hold the status line until they're done, so its redraw never goes over a prompt or what the user types.

Work that takes the terminal never runs under a status line, as its redraw would go over what it shows: a VM's shell, `ssh-add` asking for a key's passphrase (`checkAgent`, `Agent.Ensure`) and an agent's login in the VM. A launch shows its status lines around those steps rather than across them. A background process, such as the proxy, a forwarder or a link session, has no terminal and shows none.

Tests check the label rather than a terminal. A `msg.Fake` gives each status line's label, and each logged command's step, to its `OnStatus`, and `Showing` returns the one showing, so a seam that the slow work goes through records what showed meanwhile, as `TestAgentImportStatus` does with the `Opencode` and `ExcludeFromBackup` seams, and a `backendtest.Fake` gives each call to its `OnCall`. `TestLaunchStatusLines` runs a new, a running and a stopped VM's launch and fails on any backend call made with no status line showing, but the shell's, which takes the terminal, and `Running`'s, which reads the VM's pid files. `TestSSHAddOffStatusLine` fails on an `ssh-add` that runs under one, on the user's agent or devvm's own, and `chflagsCalls` fails a clone test on a lock or unlock of `.git` with none. The structure test's `status-lines` rule fails on a function that does slow work by name, a host command, host git, a walk of a folder, a volume's lookup, a request through `internal/fetch` or a backend's command outside a step, and shows no status line of its own, unless `statusSites` lists it with why, such as the status line of the caller it runs under ([The structure test](structure.md#the-structure-test)).

## Command arguments

Each command checks its arguments and flags before it does anything else, so a usage error never looks for a VM, applies the retention settings or changes a file. `internal/cli/args.go` holds the checks:

- **A command's `Args`** is one of `noArgs`, `exactArgs(n)`, `maxArgs(n)` and `minArgs(n)`, never Cobra's own validators, whose errors are in English. Each one names the arguments the command's `Use` shows (`KEY_FILE`, `[all|none|LIST]`), and with `ValidArgs` set, it refuses any other value. `noArgs` on a command with commands of its own says the word isn't one of them and lists them.
- **Values are checked in `Args` too.** A command adds its own check with `cobra.MatchAll`, such as `settings.ParsePorts` for devvm ports or `netpolicy.Rule` for devvm network allow, returning the same message the command would fail with later.
- **Flag values are checked by name** in `checkFlagValue`, because one flag name has one meaning: `--cpus`, `--memory`, `--disk`, `--ports`, `--lifetime`, `--older-than`, `--tools`, `--agents` and `--layers`. Flags that can't be given together are marked with `exclusive(cmd, a, b)`, which `checkFlags` checks first, in place of Cobra's `MarkFlagsMutuallyExclusive` alone.
- **The parser's own flag errors** (an unknown flag, one without its value, a value its type refuses) become messages in `flagError`, set as the root's `SetFlagErrorFunc`.

`checkCommands` wraps every command's `Args` in these checks when `NewRoot` builds the tree. A message one of them returns is a usage error: `Execute` prints it, then `usage_help` naming the command's own `--help`, and exits with 2. `usage_error` is only the fallback for a parser error devvm doesn't know. What a command's `RunE` returns once it ran is a failure, which `execute` shows and exits with 1. `TestUsageErrorsEveryCommand` runs every command with an unknown flag, without the argument it needs, with too many and with each flag's value blank, and checks that each is a usage error and nothing runs.

devvm itself takes no arguments, so a layer may have a command's name. The root's `Args` is `noArgs`, which for the root runs `checkWord` (`internal/cli/word.go`): a word that isn't a command is a usage error. When the word, or each name in it joined with `+` or a comma, is a part's name, from the same `partNames` that `checkPartFlag` reads (`image.Toolchains`, `image.Agents` and the build env's layers), the error is `no_command_part`, with the flags that choose them. Any other word gets the closest command, from Cobra's `SuggestionsFor` with `SuggestionsMinimumDistance` 2 (`msg.SuggestDistance`, which `msg.Closest` also uses). devvm's own help command and the root's help func run the same check, so `devvm help WORD` and `devvm WORD --help` fail as `devvm WORD` does. The help func can't return an error, so it panics with a `wordPanic` that `execute` recovers. `--tools`, `--agents` and `--layers` are checked in `checkFlagValue` through `checkPartFlag`: `none` alone, or a list of the part's names. A name of another part's is refused with that part's flag (`part_other_flag`), and any other name it doesn't know with the closest name (`vm.CheckPart`).

## The CLI rule tests

The Go tests in `internal/cli` walk the command tree, so a command that breaks one of the command line's rules fails `mise run test`, whether it's new or changed. Each exception says why, in its test's list or in the command's own `about`:

- **`TestEveryCommandsHelp`** checks that every command users see has a `Short` in sentence case without a full stop, a `Long` of one to three sentences, an `Example` and a docs page in its `about`, and that each of the root's commands is in a group. devvm help is the exception, as Cobra lists it apart and the CLI reference leaves it out. `TestAboutGroupsAreHelpGroups` checks that each group a command names is one of `helpGroups`.
- **`TestPersistentFlags`** checks that every command takes `-h/--help`, `--no-input` and `-v/--verbose` and has no other persistent flag. **`TestFlagNames`** checks that each shorthand is in its list, `-h`, `-v`, `-f`, `-g` and `-n`, for one flag only, that no command takes `--yes`, and that a flag has one help text wherever it appears, except the names in `manyUses`. `TestFlagNamesHaveOneMeaning` checks that a flag name has the same shorthand, type and value everywhere. **`TestDurationFlags`** checks that a flag whose value is a `DURATION` is read with `state.ParseDuration` in `checkFlagValue`, and that no flag is a Go `time.Duration`.
- **`TestShowCommandsTakeJSON`** checks that every command users see takes `--json` or says why not in its `about`'s `noJSON`, and `TestJSONShapes` checks that every command with `--json` has a shape.
- **`TestCommandNames`** checks that command names are lower case without aliases, that no command is named `list`, `show`, `get`, `delete`, `remove` or `ls` but devvm ls, that a command named unX has a sibling X, and that each argument in a `Use` is a `PLACEHOLDER` or a word, with `|`, `[]` and `...` only. **`TestUseMatchesArgs`** runs every command with the arguments its `Use` shows, from `argSamples`, and with none when they're optional, and checks that it runs, so its `Args` takes what its help shows. `TestUnknownWord` checks that `devvm WORD`, `devvm WORD --help` and `devvm help WORD` fail alike for a word that isn't a command, and that a toolchain's, an agent's or a layer's name, alone or joined with others, points to its flag while any other word gets the closest command.
- **`TestCommandLines`** reads every devvm command line in the repo: the docs, the generated CLI reference among them, the README, AGENTS.md, `locales/en.toml`, Go's comments and strings, `assets/`, `install.sh`, the CLI tests and their stubs, the scripts, the workflows and the skills. It checks that each one's command exists, that each flag is one that command takes, that a placeholder is one its `Use` shows and a flag's value the one its help shows, such as `DURATION`, and that a word after a command with commands of its own isn't a command from elsewhere in the tree. A command line written to be typed, in a code block, a code span or a CLI test's `exec`, also has its values checked with `checkFlagValue`, and when it has nothing to fill in, it runs in a tree whose commands do nothing, where it mustn't be a usage error. Prose has only its command and its flags checked, because its words after the command are English. The lines written to show a refusal are in `refused`, each with why, and a CLI test's `! exec` lines are left out, as they're meant to fail. `mise run lint:docs` runs it too, so a docs change that names a command that doesn't exist fails there. `TestCheckCommandLine` checks that it finds each kind of stale line.
- **`TestQuestionsDeclareAnswers`** in `internal/cli` finds every question the code makes, outside tests. A question is a `msg.Question`, which `Ask`, `Confirm`, `Answer` and `Secret` take, and only two methods of a message make one: `AnsweredBy("--force")` names the flag or command that answers it, and `RefusedWith(msg.X(...))` gives a question that no flag answers the refusal that says what to do instead. The test checks that each `AnsweredBy` names a string, that each flag in it is one its command takes and each command one devvm has, and that each `RefusedWith` gives a message, so no question is asked without its answer declared. `TestCheckAnswer` checks that it finds each kind of wrong answer.

## Messages

Every message devvm prints is a table in `locales/en.toml`, and Go code calls it through a function generated from it. `internal/msg` renders messages but never prints them; the run's `ui.UI` does. Only `cmd/devvm` and `internal/cli` hold it. Every other package prints and asks through the one `msg.Reporter` interface, which `ui.UI` implements: a package's type embeds it, its constructor takes it, and cli gives it the run's UI. A package that takes no Reporter gets the answer instead, because cli asks the question first and passes the answer in. Where a package's warnings also say where the run log is, as Lima's and the shared tools' do, its type has its own `Warn` method over the Reporter's. A Go test gives a package a `msg.Fake` instead, a Reporter that prints nothing and refuses every question unless the test sets its fields, and records what it says with a `msg.Recorder`.

Prose is never built with `fmt`: every sentence devvm shows the user is a message. A message made from an error carries that error with `.Because(err)`, rather than only its text from `err.Error()`. Data output, such as a table or `--json`, isn't a message, and neither is text in a script that runs somewhere else. Data goes to the UI's `Stdout()`, and a log line that isn't a message goes to its `Stderr()`.

The locale files are in [go-i18n](https://github.com/nicksnyder/go-i18n)'s TOML format, with only go-i18n's own message fields, so go-i18n's loader reads them as they are and its `goi18n` tools work on them:

```toml
[no_vm]
other = "There's no VM for {{.dir}}. Run devvm to make one."

[gc_tools]
one = "{{.count}} shared tool image is unused ({{.size}})."
other = "{{.count}} shared tool images are unused ({{.size}})."

[gc_tools_no_vm]
description = "tools lists the tool images, as \"node 24.21.0, claude 2.1.295\""
one = "{{.count}} current shared tool image no VM has ({{.size}}): {{.tools}}."
other = "{{.count}} current shared tool images no VM has ({{.size}}): {{.tools}}."
```

- **The ID** is the table's name, in snake_case.
- **`other`** is the text: a Go template whose fields are snake_case (`{{.vm_name}}`). Only plain fields are allowed, no logic.
- **Fields' types:** the generator makes each field a `string` parameter of the message's function, but `count`, which is an `int`, and `vm` and `vm_name`, which are `vmname.VM`. A field that may hold something other than a VM's name, such as an image's name or a VM's name as the user typed it, is named `what` or `name` instead.
- **`one`** is the text for a count of 1, when the wording changes with the count. Both texts then take `{{.count}}`.
- **`description`** is an optional note for translators.
- **Nothing else:** the generator refuses keys go-i18n doesn't know, nested tables, plural forms English doesn't use (`zero`, `two`, `few`, `many`) and `leftDelim`/`rightDelim`.
- **Voice:** a message's text follows the docs-voice skill's [Messages](https://github.com/devvmhq/devvm/blob/main/.agents/skills/docs-voice/SKILL.md#messages) rules, which set its length (an error or a warning has at most two sentences, and a prompt at most two lines of context), and the mechanism goes in its doc link rather than its text.
- **One line:** a value renders on one line without control, bidi or invisible characters, because `msg.OneLine` cleans every value as the message renders. Text from a VM that devvm shows as data goes through `msg.Printable` instead, which keeps its newlines and tabs. [Colors](#colors) says what the two drop.

### Doc links

A message links a docs page through the `docs` map in `internal/msg/docs.go`, from message ID to a page in `docs/content/docs` and a heading on it. The page is its path without `.md`, and a section's own page, its `_index.md`, is the section's folder:

```go
var docs = map[string]string{
    "vm_outdated":   "using/images#outdated-vms",
    "backend_lacks": "backends#when-a-backend-lacks-something",
}
```

devvm prints the page's address on the docs site on the line after the text.

After editing `en.toml`, run:

```sh
mise exec -- go generate ./internal/msg
```

It writes `internal/msg/messages_gen.go`, with one function per message. The parameters are the template's fields in sorted order, in camelCase, and `count int` for plural messages: `[no_vm]` becomes `msg.NoVM(dir string) msg.M`, and a value that's missing or extra is a compile error. `msg.M` is also an `error`: one made from another error carries it with `.Because(err)`, so `errors.Is` and `errors.As` see through it, while the text stays the message's own.

The tests fail when:

- `messages_gen.go` is stale;
- a message doesn't render with sample values, or shows `<no value>`;
- a generated function is never called outside the generated file and the tests;
- a doc link names a message that isn't in `en.toml`, or a page or heading that doesn't exist. `mise run lint` runs this check too.

### Questions

A prompt asks a `msg.Question`, never a bare message, so every question declares what answers it where devvm can't ask: without a terminal, with `--no-input`, or once the input ends. A message becomes a question in one of two ways. `msg.X(...).AnsweredBy("--force")` names the flag or command that answers it, and its refusal is `no_input`, which names that answer and shows the question first when the prompt wasn't printed. `RefusedWith(msg.Y(...))` makes a question that no flag answers, and its refusal is the message given, which says what to do instead. `About(...)` adds the messages said before the question, which devvm shows only when it asks.

The Reporter's `Ask`, `Confirm`, `Answer` and `Secret` take the question and, where devvm can't ask it, return its `*msg.Refusal`, an error whose `Msg` is the refusal's message. A command returns it as it would any failure, and a caller with something to fall back on does that instead, so no caller checks for a terminal itself. `TestQuestionsDeclareAnswers` checks every question in the code, as [The CLI rule tests](#the-cli-rule-tests) says, and a CLI test answers one with `ttyin` or `DEVVM_PIPED_ANSWERS=1`, as [CLI tests](tests.md#cli-tests) says.

### Names mode

With `DEVVM_MESSAGES=names`, a message prints as its ID and its values in sorted key order, so tests check which message devvm printed and with what, never its wording or language:

```txtar
! exec devvm _msg nope
stderr 'msg_unknown name=nope$'
```

Every CLI test runs in names mode unless it sets `env DEVVM_MESSAGES=`.

Names mode belongs to `internal/ui`, which reads it once, with the rest of a stream's style, in `New`: a `msg.M` itself always renders as its text, so its `String` and `Error` are the same whatever the environment says. Whatever shows a message as data, a table's cell or a `--json` field, goes through the UI's `Text`, or a `Table`'s, to get the names in names mode. `New` takes the function it reads the environment with, `os.Getenv` for devvm's own UI, so a Go test makes a UI in names mode without setting the environment and can run in parallel; cli's `testUI` does. A Go test that checks a message without a UI compares `Names`, `msg.IDOf` or `msg.NamesOf`.

`devvm _msg NAME key=value…` prints a message's text (never its names), for end-to-end tests that look for it in a VM's output. It's hidden from the help.

### Colors

Only `internal/ui` adds color, through a `style` for each stream that `onTerminal` makes, and `colorOn` decides each one: `NO_COLOR` or `DEVVM_MESSAGES=names` turns it off, then `FORCE_COLOR` or `CLICOLOR_FORCE` (but `0`) turns it on, then `TERM=dumb` turns it off, and otherwise the stream gets color only when it's a terminal. Names mode stays plain in a terminal too, so end-to-end checks that run devvm under `script` read the names as they are. A `UI` from `New`, as tests make, has no terminal, so it's plain unless the environment it reads sets `FORCE_COLOR` or `CLICOLOR_FORCE`, which is how cli's tests get a colored one.

The colors are SGR codes from the 16 base colors only, which the theme maps to its own shades. devvm uses no bright colors (90–97), because Solarized maps them to greys and bright black is Solarized Dark's background, and it uses no backgrounds, 256 colors or truecolor. The codes go on labels, links, log text and a message's values, never on the template's own words:

- A message's values are bold (`1`), on the status line and in prompts too. `msg.M.Segments` renders the template with each value between two private-use runes, `U+E000` and `U+E001`, after dropping those runes from the values, so a value can't start or end a marking of its own. It returns the text cut into segments, each one the template's words or a value, beside the doc link. Joined, the segments are `Text`, and `String` and names mode don't change. Every value goes through `msg.OneLine` as the message renders, in `Text`, `Segments` and `Names` alike, so a value has no newline or other control character, whoever made it, bold or not. The log end that `ErrorLogged` prints is the message on one line without bold, because bold inside dim would cancel it.
- `Error`'s `devvm:` label is bold red (`1;31`), and `Warn`'s is bold yellow (`1;33`).
- A doc link is underlined blue (`4;34`). `msg.M.Segments` gives the doc link apart from the text, so `ui` colors the link alone.
- `Info`'s `==>` is bold (`1`).
- The run log's end that `ErrorLogged` prints, with its `---` lines, the log text on the status line, and the log's lines that `-v` streams (`SetVerbose`: `LoggedCmd` and `LogWriter` copy the log to stderr through `msg.Printable`, with no status line) are dim (`2`).
- The done mark that a slow step leaves is green (`32`).

The codes are added after `msg.Printable` and `msg.OneLine`, so they're the only ones devvm prints. Those two are devvm's one cleaner, and `msg.Safe` is its one rule for which characters reach the terminal. They drop every character that `msg.Safe` refuses but newline and tab (which `OneLine` drops too), and a lone C1 byte. `msg.Safe` takes a graphic character in any script, such as an accented letter, a CJK character or an emoji, so a VM's file names still read as they are. It refuses the control characters (C0, DEL and C1), the bidi controls such as U+202E, which reorder the text after them so a name could read as another, the format characters, such as the zero-width space and joiners, the word joiner, the soft hyphen and the tag characters, the variation selectors, and the fillers that show as nothing, such as U+3164 and the braille blank U+2800, which could hide or fake text in a prompt. The diff of devvm agent save writes each character that `msg.Safe` refuses as its code instead, and the names of the files it saves must hold none. The status line, the run log's end, `Pick`, `devvm network log` and the port forwarder's log use them as well. The status line cuts and counts its plain text with `style.fit` before painting it, so it still fits the terminal's width and clears the right number of rows. The run log is never colored.

Tables on `Stdout()` are colored as stdout is. A command gets a `ui.Table` from `u.Table()` and adds a header, rows of `ui.Cell`s and a totals row, and cli never reads the environment itself. Each cell has a `ui.Role`: `Good` is green (`32`), `Attention` yellow (`33`), `Bad` red (`31`), `Secondary` dim (`2`), and `Plain` stays in the default color, while the header and totals rows are bold. `Table` pads each cell on its plain text before painting it, because `text/tabwriter` counts the codes' bytes as width, and a plain table is byte for byte what `text/tabwriter` writes with a padding of 2 (`TestTableLikeTabwriter`). The roles for each table's values are next to the code that writes it:

- devvm images: `current` is good, `stale`, `unfinished`, `unused` and `not built` need attention, and digests and keys are secondary.
- devvm ls: `Running` and the `.git` protections `on` and `clone` are good, `Stopped` needs attention, and any other status, such as `Broken`, is bad, as is the tier `unenforced`, while a confined tier, `internet`, `allowlist` or `none`, is good and `open` stays plain.
- devvm space: `only copy` and `leftover` in KEPT need attention.
- devvm doctor: `native` is good, `emulated` needs attention, and `unreviewed` and `none` are bad.

Every command's `--json` output is for scripts and stays plain.

`mise run test` unsets `NO_COLOR`, `FORCE_COLOR` and `CLICOLOR_FORCE`, and the end-to-end tests unset the last two, so your own settings never change what they check.

### Translations

A translation is `locales/<language>.toml` (`de.toml`) with the same tables, `one` and `other` translated (and the language's other plural forms, such as `few`), loaded by go-i18n the same way. Doc links are the same in every language: the docs site is English. devvm picks the language from the first of `LC_ALL`, `LC_MESSAGES` and `LANG` that is set (`de_DE.UTF-8` is German); `C` and `POSIX` are English. A message missing from a translation is shown in English.

## JSON output

Every command that shows or lists something takes `--json`, which `jsonFlag` in `internal/cli/json.go` adds. The command builds a struct of its own with snake_case `json` tags, next to the code that writes its table, and writes it with `printJSON`, which puts `"version": 1` (`jsonVersion`) first, writes HTML characters as they are and refuses anything but an object. A list it prints is never `nil`, so it's `[]` rather than `null`, and a field that doesn't apply is `omitempty`. Sizes are bytes unless the field's name gives the unit, as in `memory_gib`.

The root command's `PersistentPreRun` calls `u.DataOnly()` for any command given `--json`, so `Say`, `Info` and `LogNote` print to stderr from then on, and stdout gets only the JSON. A settings command that also changes its setting with a value wraps its `Args` in `showsOnly`, which refuses the value with `--json` (`json_shows_only`) before anything else runs.

`TestJSONShapes` in `internal/cli/json_test.go` holds each command's shape, a `path: type` line for each field of a sample with every field set. A field renamed, removed or given another type fails it. Shapes only gain fields, so a new field goes into the sample and the test, and any other change raises `jsonVersion`. A new command that shows or lists something gets `--json`, a row in that test, a CLI test and a section on its page with an example, which [output for scripts](../using/command-line.md#output-for-scripts) links to.
