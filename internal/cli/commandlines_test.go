package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// repoRoot is the repo's root, from this package's folder.
const repoRoot = "../.."

// commandLine is a devvm command line found in a file: the words after
// devvm, where they are, and how they're written. One written to be typed
// (code) has its flags' values and its placeholders checked too, and one
// with nothing to fill in (run) runs in the stubbed tree, where it must
// not be a usage error. Prose, such as "devvm rm deletes the VM", has only
// its command and its flags checked.
type commandLine struct {
	where     string
	words     []string
	code, run bool
}

var (
	// devvmAt finds devvm written as a command: a word of its own followed
	// by a space, not part of a path, a host name or an identifier.
	devvmAt = regexp.MustCompile(`(?:^|[^\w./~$@:{-])devvm `)
	// codeSpan is a Markdown code span.
	codeSpan = regexp.MustCompile("`([^`]+)`")
	// placeholderWord is a word that stands for what to fill in, such as
	// IMAGE, [--cpus, TOOL..., IMAGE|KEY or a shell variable.
	placeholderWord = regexp.MustCompile(`[A-Z]{2,}|\.\.\.|[\[\]|<>$*]|^N$|^GiB$`)
	// placeholderName is a placeholder in an argument, such as TOOL in
	// TOOL... or KEY=VALUE.
	placeholderName = regexp.MustCompile(`[A-Z][A-Z_]+(?:=[A-Z]+)?`)
)

// Every devvm command line in the repo is one of devvm's: in the docs,
// the README, AGENTS.md, the messages, Go's comments and strings, the
// scripts, the tests and the skills, its command exists, each flag is
// one that command takes, a flag's value and a placeholder are those its
// help shows, and a line written to be typed is no usage error.
func TestCommandLines(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// the layers the docs' examples name, so devvm WORD finds them
	layers := filepath.Join(config, "devvm", "layers")
	if err := os.MkdirAll(layers, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"foo", "bar", "mailpit", "myteam", "extra", "dotfiles"} {
		if err := os.WriteFile(filepath.Join(layers, l+".sh"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// command lines written to show what devvm refuses, each with why
	refused := map[string]string{
		"--nope":             "Contributing's testscript example of an unknown flag",
		"ls --cpus 4":        "the command line page'sexample of a new VM's flag on another command",
		"stpo":               "the command line page'sdid-you-mean example", //nolint:misspell // the typo devvm corrects
		"stpo --help":        "the command line page'sdid-you-mean example", //nolint:misspell // the typo devvm corrects
		"help stpo":          "the command line page'sdid-you-mean example", //nolint:misspell // the typo devvm corrects
		"node":               "the command line page'sexample of a toolchain's name pointing to --tools",
		"node+claude":        "the command line page'sexample of joined names pointing to their flags",
		"network --json off": "the command line page'sexample of a value refused with --json",
		"layer new docker":   "Images' example of a built-in layer's name refused",
		"ports 5173 8080":    "Troubleshooting's example of a usage error",
		"allow":              "the cli skill's example of a command in the wrong place",
		"images build":       "the cli skill's example of a command in the wrong place",
		"git-protect strict": ".git protection's and the changelog's mode devvm 0.1.0 took, refused since",
		"--snapshot DEVVM_VERSION=1.2.3 GORELEASER_CURRENT_TAG= pins=": "goreleaser's --id devvm in a testscript stub's log, not devvm",
	}
	files := commandLineFiles(t)
	checked := 0
	for _, f := range files {
		text, err := os.ReadFile(f) //nolint:gosec // G304 the repo's own files
		if err != nil {
			t.Fatal(err)
		}
		where, _ := filepath.Rel(repoRoot, f)
		for _, l := range commandLinesIn(filepath.ToSlash(where), text) {
			checked++
			line := strings.Join(l.words, " ")
			if _, ok := refused[line]; ok {
				continue
			}
			if err := checkCommandLine(t, l); err != nil {
				t.Errorf("%s: devvm %s: %v", l.where, line, err)
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d command lines found in %d files", checked, len(files))
	}
	t.Logf("%d command lines in %d files", checked, len(files))
}

// The guide in every VM (assets/guest/guide.md) is among the files
// TestCommandLines checks, and each devvm command and flag it names for
// the user to run is one of devvm's, so the guide can't drift from the CLI.
func TestGuideCommandLines(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	guide := filepath.Join(repoRoot, "assets", "guest", "guide.md")
	if !slices.Contains(commandLineFiles(t), guide) {
		t.Fatalf("%s isn't among the files TestCommandLines checks", guide)
	}
	text, err := os.ReadFile(guide) //nolint:gosec // G304 the repo's own file
	if err != nil {
		t.Fatal(err)
	}
	lines := commandLinesIn("assets/guest/guide.md", text)
	if len(lines) < 15 {
		t.Fatalf("only %d command lines in the guide", len(lines))
	}
	for _, l := range lines {
		if err := checkCommandLine(t, l); err != nil {
			t.Errorf("%s: devvm %s: %v", l.where, strings.Join(l.words, " "), err)
		}
	}
}

// The check finds each kind of stale command line, and passes the
// current ones, in prose and in code.
func TestCheckCommandLine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, tc := range []struct {
		file, text string
		bad        bool
	}{
		{"a.md", "Run devvm network allow example.com for each host.", false},
		{"a.md", "devvm rm deletes the VM, and devvm rm --force skips the question.", false},
		{"a.md", "Run `devvm ports 5173,8000-8010` to forward them.", false},
		{"a.md", "`devvm ssh-agent add KEY_FILE [--lifetime DURATION]` loads a key.", false},
		{"a.md", "```sh\ndevvm tools add python claude  # add them\n```", false},
		{"a.go", "// devvm join puts the VM in a group", true},
		{"a.go", `"run devvm export to save it"`, true},
		{"a.md", "devvm network policy shows it.", true},
		{"a.md", "devvm gc --all deletes more.", true},
		{"a.md", "`devvm gc --history --older-than AGE`", true},
		{"locales/en.toml", `other = "Run devvm gc --history --older-than AGE."`, true},
		{"a.md", "`devvm tools add TOOL,TOOL`", false},
		{"a.md", "`devvm tools add NAME...`", true},
		{"a.md", "`devvm ports maybe`", true},
		{"a.md", "```sh\ndevvm gc --older-than 30\n```", true},
		{"a.txtar", "exec devvm secret set no_pe", true},
		{"a.txtar", "! exec devvm secret set no_pe", false},
		{"a.txtar", "project devvm gc --nope", false},
		{"a.txtar", "# devvm gc --nope", true},
	} {
		var errs []error
		for _, l := range commandLinesIn(tc.file, []byte(tc.text)) {
			if err := checkCommandLine(t, l); err != nil {
				errs = append(errs, err)
			}
		}
		if bad := len(errs) > 0; bad != tc.bad {
			t.Errorf("%s %q: errors %v, want some: %v", tc.file, tc.text, errs, tc.bad)
		}
	}
}

// commandLineFiles are the repo's files that may show devvm's command
// lines, but for the generated messages, which en.toml has, and this file.
func commandLineFiles(t *testing.T) []string {
	t.Helper()
	sources := []string{
		"README.md", "AGENTS.md", "CHANGELOG.md", "SECURITY.md", "CONTRIBUTING.md", "install.sh",
		"docs/content", "locales", "internal", "cmd", "assets", "tests", "testdata/script",
		"scripts", "packaging", ".github", ".agents/skills/cli", ".agents/skills/docs-voice",
		".agents/skills/codebase-standards",
	}
	var files []string
	for _, s := range sources {
		err := filepath.WalkDir(filepath.Join(repoRoot, s), func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir(), strings.HasSuffix(path, "messages_gen.go"), strings.HasSuffix(path, "commandlines_test.go"):
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return files
}

// commandLinesIn finds the devvm command lines in a file's text, by the
// file's kind: Markdown's code blocks and code spans are code, the rest
// prose; a testscript's exec lines that must succeed are code, and those
// meant to fail are left out; anything else is prose.
func commandLinesIn(path string, text []byte) []commandLine {
	if bytes.IndexByte(text, 0) >= 0 {
		return nil
	}
	var out []commandLine
	add := func(n int, line string, at *regexp.Regexp, code, run bool) {
		for _, words := range commandWords(line, at, code) {
			out = append(out, commandLine{where: path + ":" + strconv.Itoa(n), words: words, code: code, run: run})
		}
	}
	md, txtar := strings.HasSuffix(path, ".md"), strings.HasSuffix(path, ".txtar")
	fence := ""
	for i, line := range strings.Split(string(text), "\n") {
		n, trimmed := i+1, strings.TrimSpace(line)
		switch {
		case md && strings.HasPrefix(trimmed, "```"):
			if fence == "" {
				fence = strings.TrimPrefix(trimmed, "```") + " "
			} else {
				fence = ""
			}
		case md && fence == "mermaid ":
			// a diagram's labels are prose, in its own syntax
		case md && fence != "":
			// a command line starts its line or follows a shell operator;
			// a devvm elsewhere, as in help's text, is prose; a comment
			// is left out
			if i := strings.Index(line, " #"); i >= 0 {
				line = line[:i]
			}
			for _, at := range devvmAt.FindAllStringIndex(line, -1) {
				before := strings.TrimSpace(line[:at[1]-len("devvm ")])
				typed := before == "" || strings.HasSuffix(before, "$") || strings.HasSuffix(before, "|") ||
					strings.HasSuffix(before, "&&") || strings.HasSuffix(before, ";") || strings.HasSuffix(before, "exec")
				if words := wordsAfter(line[at[1]:], typed); len(words) > 0 {
					out = append(out, commandLine{where: path + ":" + strconv.Itoa(n), words: words, code: typed, run: typed})
				}
			}
		case md:
			line = strings.ReplaceAll(line, `\|`, "|")
			for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
				if strings.HasPrefix(m[1], "devvm ") {
					// a span with a command and flags alone names them, as in
					// "`devvm network allow -g` changes yours", and isn't run
					words := wordsAfter(strings.TrimPrefix(m[1], "devvm "), true)
					_, rest := commandPath(words)
					add(n, m[1], devvmAt, true, slices.ContainsFunc(rest, func(w string) bool { return !strings.HasPrefix(w, "-") }))
				}
			}
			add(n, codeSpan.ReplaceAllString(line, "`x`"), devvmAt, false, false)
		case txtar && strings.HasPrefix(trimmed, "exec devvm __complete "):
			// Cobra's completion, whose words are a command line being typed
		case txtar && strings.HasPrefix(trimmed, "exec devvm "):
			// a script's layers are its own, so devvm IMAGE isn't run here
			words := wordsAfter(strings.TrimPrefix(trimmed, "exec devvm "), true)
			c, _ := commandPath(words)
			add(n, line, devvmAt, true, c.HasParent())
		case txtar && strings.HasPrefix(trimmed, "!"):
		case txtar && isScriptCmd(trimmed):
			// one of the scripts' own commands (catg, project…), not devvm's
		default:
			add(n, line, devvmAt, false, false)
		}
	}
	return out
}

// isScriptCmd reports whether a script's line runs one of the commands
// script_test.go adds to testscript's.
func isScriptCmd(line string) bool {
	word, _, _ := strings.Cut(line, " ")
	_, ok := scriptCmds()[word]
	return ok
}

// commandWords returns the words after each match of at in line, up to
// what ends a command line: a shell operator or a comment, and in prose,
// a quote or a sentence's punctuation too. In code, quotes are taken off.
func commandWords(line string, at *regexp.Regexp, code bool) [][]string {
	var out [][]string
	for _, loc := range at.FindAllStringIndex(line, -1) {
		if words := wordsAfter(line[loc[1]:], code); len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// wordsAfter returns the words of a command line that starts text.
func wordsAfter(text string, code bool) []string {
	var words []string
	fields := strings.Fields(text)
	if code {
		fields = shellWords(text)
	}
	for _, w := range fields {
		if !code {
			w, _, _ = strings.Cut(w, "](")
		}
		switch {
		case code && strings.HasSuffix(w, ")") && !strings.Contains(w, "("):
			return append(words, strings.TrimSuffix(w, ")"))
		case w == "", w == "|", w == "||", w == "&&", w == `\`, w == "-->", w == "devvm", strings.ContainsAny(w[:1], "#<>;&)({`\"'"):
			return words
		case strings.ContainsFunc(w, func(r rune) bool { return r > unicode.MaxASCII }):
			return words
		case !code && strings.ContainsAny(w, "`\"'\\{}"):
			// a quote ends a sentence's command, as in "run devvm rm."
			if i := strings.IndexAny(w, "`\"'\\{}"); i > 0 && !strings.Contains(w[:i], "'") {
				if w = strings.TrimRight(w[:i], ".,:?!)"); w != "" {
					words = append(words, w)
				}
			}
			return words
		case strings.HasSuffix(w, ";"):
			return append(words, strings.TrimSuffix(w, ";"))
		case !code && !strings.HasSuffix(w, "...") && strings.ContainsAny(w[len(w)-1:], ".,:?!)"):
			if w = strings.TrimRight(w, ".,:?!)"); w != "" {
				words = append(words, w)
			}
			return words
		}
		words = append(words, w)
	}
	return words
}

// shellWords splits a command line as a shell does, quotes and all: 'a b'
// is one word, without its quotes. A quote left open ends the line.
func shellWords(text string) []string {
	var words []string
	var w strings.Builder
	quote, in := rune(0), false
	for _, r := range text {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			w.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		default:
			w.WriteRune(r)
			in = true
		}
	}
	if in && quote == 0 {
		words = append(words, w.String())
	}
	return words
}

// checkCommandLine checks one command line against a new tree.
func checkCommandLine(t *testing.T, l commandLine) error {
	t.Helper()
	s := stubbed(nil)
	s.root.InitDefaultHelpCmd()
	eachCommand(s.root, func(c *cobra.Command) { c.InitDefaultHelpFlag() })
	c, rest := pathIn(s.root, l.words)
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && !l.code {
		// prose goes on after the command: only its flags are the line's
		if err := proseAfter(c, rest[0]); err != nil || c == s.root {
			return err
		}
	}
	if c.DisableFlagParsing {
		return nil
	}
	concrete, err := checkWords(c, rest, l.code)
	if err != nil || !l.run || !concrete {
		return err
	}
	if code := s.run(t, l.words...); code == usageCode {
		return fmt.Errorf("a usage error: %s", strings.TrimSpace(s.errOut.String()))
	}
	return nil
}

// commandPath is the command that words start with, in a new tree, and
// the words after it.
func commandPath(words []string) (*cobra.Command, []string) {
	u, _, _ := testUI()
	root := NewRoot(u)
	root.InitDefaultHelpCmd()
	return pathIn(root, words)
}

// pathIn is the command under root that words start with, and the words
// after it. A word of choices, such as allow|deny|rm, is the first of its
// commands when each is one.
func pathIn(root *cobra.Command, words []string) (*cobra.Command, []string) {
	c, rest := root, words
	for len(rest) > 0 {
		var sub *cobra.Command
		for i, name := range strings.Split(rest[0], "|") {
			s := subcommand(c, name)
			if s == nil {
				sub = nil
				break
			}
			if i == 0 {
				sub = s
			}
		}
		if sub == nil {
			break
		}
		c, rest = sub, rest[1:]
	}
	return c, rest
}

// subcommand is c's command named name, hidden or not.
func subcommand(c *cobra.Command, name string) *cobra.Command {
	for _, sub := range c.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}

// proseAfter checks the first word after devvm, or after a command with
// commands of its own, in prose, where most words are English: one that
// names a command elsewhere in the tree, or a command devvm doesn't
// have, is a command in the wrong place, unless the command's Use shows
// it.
func proseAfter(c *cobra.Command, w string) error {
	if !c.HasSubCommands() || slices.Contains(strings.FieldsFunc(c.Use, notWordRune), w) {
		return nil
	}
	if (isCommandName(c.Root(), w) && !slices.Contains(englishCommands(), w)) || slices.Contains(notCommands(), w) {
		return fmt.Errorf("%s isn't one of %s's commands", w, c.CommandPath())
	}
	return nil
}

// isCommandName reports whether w is the name of a command anywhere in
// root's tree, hidden ones and help included.
func isCommandName(root *cobra.Command, w string) bool {
	if w == "help" {
		return true
	}
	found := false
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			found = found || sub.Name() == w
			walk(sub)
		}
	}
	walk(root)
	return found
}

func notWordRune(r rune) bool { return (r < 'a' || r > 'z') && r != '-' }

// englishCommands are commands' names that prose uses as English after
// devvm, as in "what devvm set" or "lets devvm lock .git".
func englishCommands() []string {
	return []string{"set", "new", "use", "add", "get", "log", "lock", "help"}
}

// notCommands are words a command line might take for a command that
// devvm doesn't have: the names a noun's bare form and rm stand for, and
// nouns its tree has under others.
func notCommands() []string {
	return []string{"list", "show", "remove", "policy", "auth", "unlink", "rules", "ssh"}
}

// checkWords checks the words after a command: each flag is one it
// takes, and in code, a flag's value is one checkFlagValue takes, or the
// placeholder its help shows, and an argument's placeholder is one its
// Use shows. It reports whether there was nothing to fill in.
func checkWords(c *cobra.Command, words []string, code bool) (concrete bool, err error) {
	concrete = true
	for i := 0; i < len(words); i++ {
		w := strings.Trim(words[i], "[]")
		if w != words[i] {
			concrete = false
		}
		if !strings.HasPrefix(w, "-") || w == "-" || w == "--" {
			if placeholderWord.MatchString(words[i]) {
				concrete = false
				if code && !strings.Contains(words[i], "$") {
					if err := placeholderInUse(c, words[i]); err != nil {
						return false, err
					}
				}
			}
			continue
		}
		name, value, given := strings.Cut(w, "=")
		f := lookupFlag(c, name)
		if f == nil {
			return false, fmt.Errorf("%s takes no %s", c.CommandPath(), name)
		}
		if f.Value.Type() == "bool" {
			continue
		}
		if !given {
			if i+1 == len(words) {
				// a flag named alone, as in "devvm gc --older-than takes"
				concrete = false
				continue
			}
			i++
			value = strings.Trim(words[i], "[]")
		}
		switch form := flagForm(f); {
		case value != form && placeholderName.MatchString(value) && strings.ToUpper(value) == value && !strings.Contains(value, "$") && !capitalValues(f.Name):
			return false, fmt.Errorf("%s's value is %s, not %s", name, form, value)
		case !code:
		case capitalValues(f.Name) && value != form:
			if err := checkFlagValue(f.Name, value); err != nil {
				return false, err
			}
		case placeholderWord.MatchString(value) || value == form:
			concrete = false
		default:
			if err := checkFlagValue(f.Name, value); err != nil {
				return false, err
			}
		}
	}
	return concrete, nil
}

// capitalValues reports whether the flag's values are written in capitals,
// so one isn't taken for a placeholder: --env's, a variable's name, such
// as STRIPE_API_KEY, and --method's, HTTP methods, such as GET,HEAD.
func capitalValues(flag string) bool { return flag == "env" || flag == "method" }

// lookupFlag is c's flag, its own or inherited, as written: --name or -n.
func lookupFlag(c *cobra.Command, written string) *pflag.Flag {
	if name, ok := strings.CutPrefix(written, "--"); ok {
		if f := c.Flags().Lookup(name); f != nil {
			return f
		}
		return c.InheritedFlags().Lookup(name)
	}
	short := strings.TrimPrefix(written, "-")
	if len(short) != 1 {
		return nil
	}
	if f := c.Flags().ShorthandLookup(short); f != nil {
		return f
	}
	return c.InheritedFlags().ShorthandLookup(short)
}

// placeholderInUse checks that each placeholder in an argument, such as
// TOOL... or [all|none|LIST], is one c's Use shows.
func placeholderInUse(c *cobra.Command, word string) error {
	// WORD and COMMAND stand for any word, in lines about devvm WORD and help
	shown := append(placeholderName.FindAllString(c.Use, -1), "WORD", "COMMAND")
	for _, s := range shown {
		k, _, _ := strings.Cut(s, "=")
		shown = append(shown, k)
	}
	for _, p := range placeholderName.FindAllString(word, -1) {
		if !slices.Contains(shown, p) {
			return fmt.Errorf("%s isn't an argument of %s, whose Use is %q", p, c.CommandPath(), c.Use)
		}
	}
	return nil
}
