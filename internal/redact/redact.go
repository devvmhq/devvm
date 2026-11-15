// Package redact keeps passwords out of logs: a proxy key of devvm's in a proxy URL, and the
// password in any URL's user info, such as a company proxy's in the shell's HTTPS_PROXY, with a
// scheme or without one (user:password@host:port). String takes them out of a line on its way to
// devvm's run log, and Split says how much of a line still to be ended a writer may write
// first; File takes them out of a log another program keeps writing (Lima's hostagent's). Secret
// (secret.go) is the type of every such value in devvm's own code, which never prints as itself.
package redact

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

// proxyKey is a VM's proxy key (32 hex digits) or the build proxy's password (64) in a proxy
// URL, which lets a process act as that VM or build.
var proxyKey = regexp.MustCompile(`devvm:([0-9a-f]{32,64})@`)

// urlPassword is the password in a URL's user info (scheme://user:password@).
var urlPassword = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s/:@"'` + "`" + `]*):([^\s@/?#]+)@`)

// barePassword is the password in a proxy URL without a scheme (user:password@host:port), as in
// HTTPS_PROXY=alice:pw@proxy.corp:8080. It needs the port, and the user must start a word, so
// that ordinary text with an "@" (an email, an ssh target such as git@github.com:org/repo, a
// mailto: link) isn't taken for one.
var barePassword = regexp.MustCompile(`(^|[\s"'` + "`" + `=(,;<\[{])([^\s/:@"'` + "`" + `=(,;<\[{]+):([^\s@/?#]+)@(?:[a-zA-Z0-9.-]+|\[[0-9a-fA-F:.]+\]):[0-9]{1,5}\b`)

// secrets are the patterns whose last group is a secret, in the order String applies them: the
// proxy key, then the passwords.
func secrets() []*regexp.Regexp {
	return []*regexp.Regexp{proxyKey, urlPassword, barePassword}
}

// keyStandIn and passwordStandIn are what String writes for a proxy key and another password.
const (
	keyStandIn      = "<key>"
	passwordStandIn = "<password>"
)

// String is s with each proxy key written as devvm:<key>@ and each other URL password as
// user:<password>@, in a URL with a scheme or a proxy URL without one.
func String(s string) string {
	s = proxyKey.ReplaceAllString(s, "devvm:"+keyStandIn+"@")
	for _, re := range secrets()[1:] {
		s = replaceLast(s, re)
	}
	return s
}

// replaceLast is s with the last group of each match of re, a password, written as <password>,
// but for a proxy key's stand-in.
func replaceLast(s string, re *regexp.Regexp) string {
	var b strings.Builder
	done := 0
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[len(m)-2], m[len(m)-1]
		if s[start:end] == keyStandIn {
			continue
		}
		b.WriteString(s[done:start])
		b.WriteString(passwordStandIn)
		done = end
	}
	b.WriteString(s[done:])
	return b.String()
}

// Split is where a writer may end what it writes of b, the start of a line whose end is still to
// come, so that no secret is cut in two and the rest of each is found once the line goes on:
// after b's last space or tab, as a URL and its password have none, but at most tail bytes
// before b's end, and never inside a secret already in b. What follows Split is for the writer
// to hold, with the rest of the line.
func Split(b []byte, tail int) int {
	i := max(bytes.LastIndexAny(b, " \t\f")+1, len(b)-tail, 0)
	for moved := true; moved; {
		moved = false
		for _, re := range secrets() {
			for _, m := range re.FindAllIndex(b, -1) {
				if m[0] < i && i < m[1] {
					i, moved = m[0], true
				}
			}
		}
	}
	return i
}

// fileMax is as much of a log as File reads: a hostagent's log right after its start is far
// smaller.
const fileMax = 64 << 20

// File overwrites each proxy key and URL password in the log at path with as many "*", in place:
// the file keeps its length and every other byte, so a program that is still writing to it at
// its own offset (Lima's hostagent, which opened it without O_APPEND) goes on where it was. A
// missing file has nothing to redact.
func File(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // a log in devvm's own folders
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, fileMax))
	if err == nil {
		err = overwrite(f, spans(b))
	}
	return errors.Join(err, f.Close())
}

// spans are the offsets of each secret in b, start and end.
func spans(b []byte) [][2]int {
	var out [][2]int
	for _, re := range secrets() {
		for _, m := range re.FindAllSubmatchIndex(b, -1) {
			// The secret is the last group: the key, or the password.
			start, end := m[len(m)-2], m[len(m)-1]
			out = append(out, [2]int{start, end})
		}
	}
	return out
}

// overwrite writes "*" over each span of f.
func overwrite(f io.WriterAt, spans [][2]int) error {
	for _, s := range spans {
		if _, err := f.WriteAt(bytes.Repeat([]byte{'*'}, s[1]-s[0]), int64(s[0])); err != nil {
			return err
		}
	}
	return nil
}
