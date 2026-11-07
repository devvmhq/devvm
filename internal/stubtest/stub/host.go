package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/stubtest/stubstate"
)

// mise stands in for the host's mise (DEVVM_HOST_MISE). It logs every
// call to $STUB_LOG, when that's set. mise lock writes $MISE_STUB_LOCK as
// ./mise.lock, or $MISE_STUB_OLDER_LOCK for a mise.toml listing versions
// (the step back's), whose mise.toml it copies to $MISE_STUB_OLDER_CONFIG
// when that's set; with MISE_STUB_FAIL set, it fails like mise
// offline. mise ls-remote prints $MISE_STUB_VERSIONS, one a line.
// Anything else succeeds.
func mise(c *call) int {
	c.log()
	switch c.arg(0) {
	case "ls-remote":
		c.print(strings.Join(strings.Fields(c.getenv("MISE_STUB_VERSIONS")), "\n") + "\n")
		return 0
	case "lock":
	default:
		return 0
	}
	if c.getenv("MISE_STUB_FAIL") != "" {
		c.say("mise ERROR failed to fetch versions: offline")
		return 1
	}
	lock := c.getenv("MISE_STUB_LOCK")
	if toml, err := readFile("mise.toml"); err == nil && bytes.Contains(toml, []byte("= [")) {
		lock = c.getenv("MISE_STUB_OLDER_LOCK")
		if keep := c.getenv("MISE_STUB_OLDER_CONFIG"); keep != "" {
			if err := writeFile(keep, toml); err != nil {
				return c.status(err)
			}
		}
	}
	b, err := readFile(lock)
	if err != nil {
		return c.status(err)
	}
	return c.status(writeFile("mise.lock", b))
}

// open stands in for macOS's open: it never opens anything. It logs
// every call to $STUB_LOG, and fails with $STUB_DIR/open-fails.
func open(c *call) int {
	c.log()
	if c.on("open-fails") {
		return 1
	}
	return 0
}

// code stands in for VS Code's code command: never the real editor. It
// logs every call to $STUB_LOG, and the SSH agent it was given
// ($SSH_AUTH_SOCK, or "unset") to $STUB_DIR/code-agent.
func code(c *call) int {
	c.log()
	return c.status(c.put("code-agent", []byte(c.agent()+"\n")))
}

// qemuImg stands in for qemu-img. It logs every call to $STUB_LOG. info
// says the disk is qcow2, as JSON, backed by the file named in
// $STUB_DIR/backing-<instance> (the disk's folder's name), if any;
// create makes the file.
func qemuImg(c *call) int {
	c.log()
	switch c.arg(0) {
	case "info":
		info := `{"format":"qcow2"}`
		if b, ok := c.text("backing-" + filepath.Base(filepath.Dir(c.last()))); ok {
			info = `{"format":"qcow2","backing-filename":"` + b + `"}`
		}
		c.print(info + "\n")
	case "create":
		return c.status(writeFile(c.last(), nil))
	}
	return 0
}

// swVers stands in for macOS's sw_vers: -productVersion prints
// $STUB_SW_VERS when that's set. Anything else runs the real sw_vers.
func swVers(c *call) int {
	if v := c.getenv("STUB_SW_VERS"); c.arg(0) == "-productVersion" && v != "" {
		c.print(v + "\n")
		return 0
	}
	return c.real([]string{"/usr/bin"}, "sw_vers")
}

// uname stands in for uname: -s prints $STUB_UNAME_S and -m prints
// $STUB_UNAME_M, when they're set. Anything else runs the real uname.
func uname(c *call) int {
	v := map[string]string{"-s": c.getenv("STUB_UNAME_S"), "-m": c.getenv("STUB_UNAME_M")}[c.arg(0)]
	if v != "" {
		c.print(v + "\n")
		return 0
	}
	return c.real([]string{"/usr/bin", "/bin"}, "uname")
}

// real runs the real command name, from the first of dirs that has it.
func (c *call) real(dirs []string, name string) int {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return c.replace(append([]string{p}, c.args...), nil)
		}
	}
	c.say(name + ": not found")
	return 127
}

// tmutil stands in for macOS's tmutil, so tests never change the real
// Time Machine settings: isexcluded PATH says "[Excluded]" for a folder
// the state leaves out, else "[Included]"; addexclusion PATH logs the
// call to $STUB_LOG and leaves PATH out.
func tmutil(c *call) int {
	switch c.arg(0) {
	case "isexcluded":
		st, err := c.state()
		if err != nil {
			return c.status(err)
		}
		mark := "[Included]"
		if slices.Contains(st.Excluded, c.arg(1)) {
			mark = "[Excluded]"
		}
		c.print(mark + "    " + c.arg(1) + "\n")
	case "addexclusion":
		c.log()
		return c.status(c.update(func(st *stubstate.State) error {
			st.Excluded = append(st.Excluded, c.arg(1))
			return nil
		}))
	}
	return 0
}
