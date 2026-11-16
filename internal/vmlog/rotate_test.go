package vmlog

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// A log devvm's process appends to (Capped): rotated before the write
// that would take it past its cap, the whole log copied when it holds no
// more, and what reaches it another way (the process's last error)
// counted from the file.
func TestCapped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ports.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	c := NewCapped(f, path, 20)
	for _, s := range []string{"started 0123\n", "loaded\n", "failed upstream\n"} { // 13, 20, then past
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString("devvm: the last error\n"); err != nil { // 38: past, unseen by c
		t.Fatal(err)
	}
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "started 0123\nloaded\n" || b != "failed upstream\ndevvm: the last error\n" {
		t.Errorf(".1 %q, log %q", a, b)
	}
	if _, err := c.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	// The log was past its cap: its last 20 bytes, from a line's start
	// unless that would leave nothing.
	if a, b := readLog(t, path+".1"), readLog(t, path); a != "vvm: the last error\n" || b != "next\n" {
		t.Errorf(".1 %q, log %q", a, b)
	}
	fi, err := os.Stat(path + ".1")
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf(".1: %v, %v", fi, err)
	}
}

// A log another program writes at its own offset (Lima's hostagent and
// serial console, opened without O_APPEND): once it holds more than its
// cap, its end goes to .1 with the passwords in it replaced, and it's
// emptied; the writer goes on past a hole, which counts for nothing, so
// the log isn't rotated again until it holds as much again.
func TestTrimHole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ha.stderr.log")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // the test's own file, as Lima makes it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	const limit = 64 << 10
	key := strings.Repeat("0123456789abcdef", 2)
	line := "proxy http://devvm:" + key + "@192.168.5.2:3129 serving\n"
	var all strings.Builder
	for all.Len() <= limit+len(line) {
		all.WriteString(line)
	}
	if _, err := w.WriteString(all.String()); err != nil {
		t.Fatal(err)
	}
	if done, err := Trim(path, 2*limit); done || err != nil {
		t.Fatalf("under its cap: %v, %v", done, err)
	}
	if done, err := Trim(path, limit); !done || err != nil {
		t.Fatalf("past its cap: %v, %v", done, err)
	}
	old := readLog(t, path+".1")
	if strings.Contains(old, key) || !strings.HasPrefix(old, "proxy http://devvm:"+strings.Repeat("*", len(key))+"@") || len(old) > limit {
		t.Errorf(".1 not redacted, not from a line's start, or too long: %d bytes, starting %q", len(old), old[:min(len(old), 40)])
	}
	if s := readLog(t, path); s != "" {
		t.Errorf("not emptied: %d bytes", len(s))
	}
	if _, err := w.WriteString("later\n"); err != nil {
		t.Fatal(err)
	}
	if done, err := Trim(path, limit); done || err != nil {
		t.Errorf("a hole counted: %v, %v", done, err)
	}
	if s := readLog(t, path); !strings.HasSuffix(s, "later\n") || strings.Trim(strings.TrimSuffix(s, "later\n"), "\x00") != "" {
		t.Errorf("the writer's offset: %q", s[len(s)-min(len(s), 20):])
	}
	// A copy reaching back into the hole leaves its zeros out.
	if err := Rotate(path, limit); err != nil {
		t.Fatal(err)
	}
	if old := readLog(t, path+".1"); old != "later\n" {
		t.Errorf(".1 %q", old[:min(len(old), 20)])
	}
	if _, err := w.WriteString(strings.Repeat("x", limit) + "\n"); err != nil {
		t.Fatal(err)
	}
	if done, err := Trim(path, limit); !done || err != nil {
		t.Fatalf("past its cap again: %v, %v", done, err)
	}
	if old := readLog(t, path+".1"); bytes.ContainsRune([]byte(old), 0) || old != strings.Repeat("x", limit-1)+"\n" {
		t.Errorf(".1 holds the hole, or not the end: %d bytes", len(old))
	}
}

// Nothing to trim: a missing log; and what isn't a regular file isn't
// rotated.
func TestTrimNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if done, err := Trim(filepath.Join(dir, "missing.log"), 1); done || err != nil {
		t.Errorf("missing: %v, %v", done, err)
	}
	if err := Rotate(dir, 1); err == nil {
		t.Error("a folder rotated")
	}
	if _, err := os.Stat(dir + ".1"); !os.IsNotExist(err) {
		t.Errorf("a folder's copy: %v", err)
	}
	// A log that's a link, or another file's hard link, as a VM's proxy's child could leave one
	// in the VM's logs folder: never emptied, nor is what it leads to.
	mine := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(mine, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, plant := range []func(string) error{func(p string) error { return os.Symlink(mine, p) }, func(p string) error { return os.Link(mine, p) }} {
		p := filepath.Join(dir, "planted.log")
		_ = os.Remove(p)
		if err := plant(p); err != nil {
			t.Fatal(err)
		}
		if _, err := Trim(p, 1); err == nil {
			t.Error("a planted log trimmed")
		}
	}
	if b, _ := os.ReadFile(mine); len(b) != 100 { //nolint:gosec // the test's own file
		t.Errorf("the planted logs' target: %d bytes", len(b))
	}
}

// Keep looks at its logs until ctx ends, rotating each past its cap.
func TestKeep(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logs := []string{filepath.Join(dir, "serial.log"), filepath.Join(dir, "missing.log")}
	if err := os.WriteFile(logs[0], []byte(strings.Repeat("boot\n", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Keep(ctx, logs, 100, 10*time.Millisecond)
	}()
	for deadline := time.Now().Add(10 * time.Second); readLog(t, logs[0]) != ""; {
		if time.Now().After(deadline) {
			t.Fatal("not rotated")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if old := readLog(t, logs[0]+".1"); old != strings.Repeat("boot\n", 20) {
		t.Errorf(".1 %q", old)
	}
	if _, err := os.Stat(logs[1]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing log made: %v", err)
	}
}

// A VM rebooted in place (QEMU) traces its boot to its console again,
// the proxy's password with it: Keep replaces it as the log grows, under
// its cap, and goes on after a rotation, past the hole it left.
func TestKeepRedacts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serial.log")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // the test's own file, as QEMU makes it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	key := strings.Repeat("0123456789abcdef", 2)
	traced := "+ echo HTTPS_PROXY=http://devvm:" + key + "@192.168.5.2:3129\n"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Keep(ctx, []string{path}, 4<<10, 10*time.Millisecond)
	}()
	defer func() { cancel(); <-done }()
	waitRedacted := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); strings.Contains(readLog(t, path), key); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not redacted", what)
			}
		}
	}
	for _, s := range []string{"boot\n", traced, "reboot\n", traced} {
		if _, err := w.WriteString(s); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitRedacted("written twice")
	// Past its cap, rotated, then written again past the hole.
	if _, err := w.WriteString(strings.Repeat("boot\n", 1<<10)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); readLog(t, path+".1") == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("not rotated")
		}
	}
	if _, err := w.WriteString(traced); err != nil {
		t.Fatal(err)
	}
	waitRedacted("after a rotation")
}

// A log that hasn't grown since the last look costs that look nothing,
// and one that grew is read again only MatchMax back, not from the start
// of a long line that hasn't ended: what's further back is left as the
// earlier look found it (here, a secret put there behind its back, which
// a look that read it again would replace).
func TestKeptStillLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serial.log")
	line := "@" + strings.Repeat("x", 1<<20)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &kept{path: path, limit: VMMax}
	k.look()
	if k.at != (redact.Look{Line: 0, Read: int64(len(line))}) || k.size != int64(len(line)) {
		t.Fatalf("after the first look: %+v", k)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	secret := " http://u:e2e-not-a-secret@h" //nolint:gosec // a fake password
	if _, err := f.WriteAt([]byte(secret), 10); err != nil {
		t.Fatal(err)
	}
	k.look()
	if !strings.Contains(readLog(t, path), "e2e-not-a-secret") {
		t.Error("a log that didn't grow was read again")
	}
	if _, err := f.WriteAt([]byte("y\n"), int64(len(line))); err != nil {
		t.Fatal(err)
	}
	k.look()
	if !strings.Contains(readLog(t, path), "e2e-not-a-secret") || k.at.Line != int64(len(line))+2 {
		t.Errorf("a long line read again from its start, or its end not seen: %+v", k.at)
	}
}

// A rotation that cuts a log in the middle of a secret whose "@" hasn't
// come yet doesn't copy its start to .1 as it is.
func TestRotateCutSecret(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serial.log")
	key := strings.Repeat("0123456789abcdef", 2)
	if err := os.WriteFile(path, []byte(strings.Repeat("boot\n", 100)+"+ echo http://devvm:"+key[:20]), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(path, 1<<10); err != nil {
		t.Fatal(err)
	}
	if old := readLog(t, path+".1"); strings.Contains(old, key[:20]) || !strings.HasSuffix(old, "devvm:"+strings.Repeat("*", 20)) {
		t.Errorf(".1 ends %q", old[len(old)-min(len(old), 40):])
	}
}

// The rest of a key that a rotation cut, which the log's writer then
// writes at its own offset past the hole, is starred up to its "@" once
// it comes; and a log whose data starts at 0 again after a rotation (a
// writer that appends) is read afresh, from its start.
func TestKeptCutAndAppend(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serial.log")
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // the test's own file, as QEMU makes it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	key := strings.Repeat("0123456789abcdef", 2)
	if _, err := w.WriteString(strings.Repeat("boot\n", 1<<10) + "+ echo http://devvm:" + key[:20]); err != nil {
		t.Fatal(err)
	}
	k := &kept{path: path, limit: 1 << 10}
	k.look()
	if !k.at.Cut || k.hole == 0 || strings.Contains(readLog(t, path+".1"), key[:20]) {
		t.Fatalf("not rotated with its cut: %+v", k)
	}
	if _, err := w.WriteString(key[20:] + "@192.168.5.2:3129\nlater\n"); err != nil {
		t.Fatal(err)
	}
	k.look()
	if s := readLog(t, path); strings.Contains(s, key[20:]) || !strings.HasSuffix(s, strings.Repeat("*", len(key)-20)+"@192.168.5.2:3129\nlater\n") || k.at.Cut {
		t.Errorf("the key's rest: %q, %+v", s[len(s)-min(len(s), 60):], k.at)
	}
	// Emptied and written again from its start, by appending, past where
	// the hole would end: data at 0 says it's no writer at its offset.
	hole := k.hole
	if err := os.WriteFile(path, []byte("x http://a:e2e-not-a-secret@h:1\n"+strings.Repeat("y", int(hole))), 0o600); err != nil {
		t.Fatal(err)
	}
	k.limit = 1 << 20 // under its cap now
	k.look()
	if s := readLog(t, path); strings.Contains(s, "e2e-not-a-secret") || k.hole != 0 {
		t.Errorf("appended from 0: log starts %q, %+v", s[:min(len(s), 30)], k.at)
	}
}

// On a file system without holes, what a rotation emptied reads as real
// zeros and DataStart says 0: the keeper goes on past them from where it
// emptied the log, rather than reading it afresh and rotating again.
func TestKeptNoHoles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "serial.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("boot\n", 1<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &kept{path: path, limit: 1 << 10}
	k.look()
	hole, old := k.hole, readLog(t, path+".1")
	if hole == 0 || old == "" {
		t.Fatalf("not rotated: %+v", k)
	}
	// The writer goes on at its offset, the emptied part written out as zeros.
	line := "+ echo http://devvm:" + strings.Repeat("0123456789abcdef", 2) + "@192.168.5.2:3129\n"
	if err := os.WriteFile(path, append(make([]byte, hole), line...), 0o600); err != nil {
		t.Fatal(err)
	}
	k.look()
	s := readLog(t, path)
	if k.hole != hole || readLog(t, path+".1") != old || strings.Contains(s, "0123456789abcdef") || !strings.HasSuffix(s, strings.Repeat("*", 32)+"@192.168.5.2:3129\n") {
		t.Errorf("read afresh or not redacted: %+v, ends %q", k, s[len(s)-min(len(s), 60):])
	}
}

// Redact replaces each secret in place in a log's file, refuses what
// isn't a regular file, a link to one included, and has nothing to do for
// a missing log; Trim first removes what a rotation killed midway left.
func TestRedactAndTemps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ha.stderr.log")
	if err := os.WriteFile(path, []byte("HTTPS_PROXY=http://alice:e2e-not-a-secret@h:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Redact(path); err != nil || strings.Contains(readLog(t, path), "e2e-not-a-secret") {
		t.Errorf("Redact: %v, %q", err, readLog(t, path))
	}
	if err := Redact(filepath.Join(dir, "missing.log")); err != nil {
		t.Errorf("missing: %v", err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := Redact(link); err == nil {
		t.Error("followed a link")
	}
	if err := Redact(dir); err == nil {
		t.Error("a folder: no error")
	}
	tmp := filepath.Join(dir, ".ha.stderr.log.1.tmp-123")
	if err := os.WriteFile(tmp, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Trim(path, VMMax); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a stale copy stays: %v", err)
	}
}
