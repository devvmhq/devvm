package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/stubtest/stubstate"
)

// A call is one run of a stub: the command it's run as, its arguments
// and environment, its streams, and the process that ran it.
type call struct {
	ctx    context.Context
	name   string
	args   []string
	lookup func(string) (string, bool) // os.LookupEnv
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	ppid   int
	// replace runs argv in the stub's place, its environment with extra
	// added, and returns its exit status (a real exec, from main).
	replace func(argv, extra []string) int
	// sleep waits, for a stub that hangs.
	sleep func(time.Duration)
	// switches is the stubs' folder, from STUB_DIR.
	switches
}

// stubs are the commands the binary stands in for, by name.
func stubs() map[string]func(*call) int {
	return map[string]func(*call) int{
		"chflags":             logged,
		"code":                code,
		"docker":              docker,
		"limactl":             limactl,
		"mise":                mise,
		"open":                open,
		"opencode":            opencode,
		"osascript":           osascript,
		"pbcopy":              clipCopy,
		"qemu-img":            qemuImg,
		"qemu-system-aarch64": logged,
		"qemu-system-x86_64":  logged,
		"security":            security,
		"ssh":                 ssh,
		"ssh-add":             sshAdd,
		"ssh-agent":           sshAgent,
		"ssh-keygen":          sshKeygen,
		"stub":                control,
		"sw_vers":             swVers,
		"tmutil":              tmutil,
		"uname":               uname,
		"wl-copy":             clipCopy,
		"wl-paste":            wlPaste,
		"xclip":               xclip,
	}
}

// run runs the stub the call names and returns its exit status.
func (c *call) run() int {
	if c.replace == nil {
		c.replace = replaceProcess
	}
	if c.sleep == nil {
		c.sleep = time.Sleep
	}
	c.switches = switches(c.getenv("STUB_DIR"))
	stub, ok := stubs()[c.name]
	if !ok {
		c.say("stub: no stub " + c.name)
		return 127
	}
	return stub(c)
}

// logged only logs the call: chflags (which sets no flags, so test
// folders stay deletable) and QEMU (there so a Linux host passes
// devvm's check for it).
func logged(c *call) int {
	c.log()
	return 0
}

// getenv is the variable's value, "" when it isn't set.
func (c *call) getenv(name string) string {
	v, _ := c.lookup(name)
	return v
}

// agent is the SSH agent the call was given ($SSH_AUTH_SOCK), or "unset".
func (c *call) agent() string {
	if v, ok := c.lookup("SSH_AUTH_SOCK"); ok {
		return v
	}
	return "unset"
}

// joined is the call's arguments joined by spaces, as a shell's "$*".
func (c *call) joined() string { return strings.Join(c.args, " ") }

// arg is the call's i-th argument, "" past its last.
func (c *call) arg(i int) string {
	if i < len(c.args) {
		return c.args[i]
	}
	return ""
}

// last is the call's last argument, "" without any.
func (c *call) last() string { return c.arg(len(c.args) - 1) }

// after is the value of the last flag among the arguments, "" without
// one.
func (c *call) after(flag string) string {
	v := ""
	for i, a := range c.args {
		if a == flag {
			v = c.arg(i + 1)
		}
	}
	return v
}

// has reports whether the arguments hold word as a word of their own.
func (c *call) has(word string) bool { return slices.Contains(c.args, word) }

// mentions reports whether the switch name is there and the arguments
// hold its text.
func (c *call) mentions(name string) bool {
	t, ok := c.text(name)
	return ok && strings.Contains(c.joined(), t)
}

// switches is the stubs' folder, STUB_DIR: the files a test makes there
// are the stubs' switches, and the files the stubs write there are what
// a test checks.
type switches string

// dir is the folder.
func (s switches) dir() string { return string(s) }

// path is the file name in the folder.
func (s switches) path(name string) string { return filepath.Join(string(s), name) }

// on reports whether the switch name is there.
func (s switches) on(name string) bool { return exists(s.path(name)) }

// text is the switch name's text without its trailing line breaks, as a
// shell's $(cat FILE) reads it, and whether it's there.
func (s switches) text(name string) (string, bool) {
	b, err := readFile(s.path(name))
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(b), "\n"), true
}

// textOr is the switch name's text, or def without it.
func (s switches) textOr(name, def string) string {
	if t, ok := s.text(name); ok {
		return t
	}
	return def
}

// raw is the switch name's contents, or def without it.
func (s switches) raw(name, def string) []byte {
	b, err := readFile(s.path(name))
	if err != nil {
		return []byte(def)
	}
	return b
}

// put writes data to the file name in the folder.
func (s switches) put(name string, data []byte) error { return writeFile(s.path(name), data) }

// touch makes the empty file name in the folder.
func (s switches) touch(name string) { _ = s.put(name, nil) }

// hang marks that it got there ($STUB_DIR/hanging), then waits 30
// seconds, as a stuck command, for the test to end it.
func (c *call) hang() int {
	c.touch("hanging")
	c.sleep(30 * time.Second)
	return 0
}

// say writes a line to stderr.
func (c *call) say(line string) { _, _ = io.WriteString(c.stderr, line+"\n") }

// print writes text to stdout.
func (c *call) print(text string) { _, _ = io.WriteString(c.stdout, text) }

// log logs the call, its name and arguments, to $STUB_LOG, with words
// after them.
func (c *call) log(words ...string) {
	c.logLine(c.getenv("STUB_LOG"), strings.Join(slices.Concat([]string{c.name}, c.args, words), " "))
}

// logLine appends line to file in one write, so lines written at once
// never mix: each write to a file opened for appending lands whole. No
// file logs nothing.
func (c *call) logLine(file, line string) {
	if file == "" {
		return
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644) //nolint:gosec // the test's own log, which tests read
	if err != nil {
		c.say("stub: " + err.Error())
		return
	}
	if _, err := f.Write([]byte(line + "\n")); err != nil {
		c.say("stub: " + err.Error())
	}
	_ = f.Close()
}

// update changes the stubs' state under its lock.
func (c *call) update(fn func(*stubstate.State) error) error {
	return stubstate.Update(c.dir(), fn)
}

// state reads the stubs' state.
func (c *call) state() (*stubstate.State, error) { return stubstate.Read(c.dir()) }

// copyIn writes the call's input to the file name in the stubs' folder.
func (c *call) copyIn(name string) error {
	b, err := io.ReadAll(c.stdin)
	if err != nil {
		return err
	}
	return c.put(name, b)
}

// drain reads the call's input to its end.
func (c *call) drain() { _, _ = io.Copy(io.Discard, c.stdin) }

// exists reports whether path is there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// isRegular reports whether path is a regular file.
func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// readFile reads a file the test or a stub wrote.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // the test's own files
}

// writeFile writes a file as a command would, readable by others, making
// the folders it's in.
func writeFile(path string, data []byte) error {
	if err := mkdir(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644) //nolint:gosec // as the command it stands for writes it
}

// mkdir makes a folder and its parents, as mkdir -p does.
func mkdir(path string) error {
	return os.MkdirAll(path, 0o755) //nolint:gosec // as the command it stands for makes it
}

// errStatus is a failure that is only an exit status.
type errStatus int

func (e errStatus) Error() string { return "exit status" }

// status is err's exit status: an errStatus's own, else 1 after saying
// it.
func (c *call) status(err error) int {
	if err == nil {
		return 0
	}
	if s, ok := errors.AsType[errStatus](err); ok {
		return int(s)
	}
	c.say(c.name + " stub: " + err.Error())
	return 1
}
