// Package redact keeps passwords out of logs: a proxy key of devvm's in a proxy URL, and the
// password in any URL's user info, such as a company proxy's in the shell's HTTPS_PROXY, with a
// scheme or without one (user:password@host:port). String takes them out of a line on its way to
// devvm's run log, and Split says how much of a line still to be ended a writer may write
// first; In takes them out of a log another program keeps writing (Lima's hostagent's and
// consoles'), as it grows, and Unended out of the cut end of a copy of one. Secret
// (secret.go) is the type of every such value in devvm's own code, which never prints as itself.
package redact

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
)

// proxyKey is a VM's proxy key (32 hex digits) or the build proxy's password (64) in a proxy
// URL, which lets a process act as that VM or build.
var proxyKey = regexp.MustCompile(`devvm:([0-9a-f]{32,64})@`)

// urlPassword is the password in a URL's user info (scheme://user:password@), of any length, as a
// token in a URL can run to kilobytes.
var urlPassword = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s/:@"'` + "`" + `]*):([^\s@/?#]+)@`)

// barePassword is the password in a proxy URL without a scheme (user:password@host:port), as in
// HTTPS_PROXY=alice:pw@proxy.corp:8080. It needs the port, and the user must start a word, so
// that ordinary text with an "@" (an email, an ssh target such as git@github.com:org/repo, a
// mailto: link) isn't taken for one.
var barePassword = regexp.MustCompile(`(^|[\s"'` + "`" + `=(,;<\[{])([^\s/:@"'` + "`" + `=(,;<\[{]+):([^\s@/?#]+)@(?:[a-zA-Z0-9.-]+|\[[0-9a-fA-F:.]+\]):[0-9]{1,5}\b`)

// MatchMax is how much of what a look at a log that grows (In) read before it reads again, from
// the start of a line not yet ended when that's within it: a secret whose end came since is
// found whole when it's shorter, and a longer one has its part in what's read starred instead
// (Leading).
const MatchMax = 64 << 10

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

// Look is how far a log that grows has been redacted (In): Line is where its last line not yet
// ended starts, and Read how far it was read. Cut says that what's at Line may go on a secret
// whose start went before it, such as one a rotation's copy took the start of (Unended). The zero
// Look is a log not read yet.
type Look struct {
	Line, Read int64
	Cut        bool
}

// leading is the start of b as the rest of a secret whose start went before it: past any zeros
// (a hole), the run of what a password or a key can hold, up to its "@". It returns the run's
// span, which holds a secret's rest only when ended says an "@" ends it, and open when b ends
// first.
func leading(b []byte) (span [2]int, ended, open bool) {
	start := len(b) - len(bytes.TrimLeft(b, "\x00"))
	i := start
	for i < len(b) && !bytes.ContainsRune([]byte(" \t\r\n\f\v@/?#"), rune(b[i])) {
		i++
	}
	switch {
	case i == len(b):
		return [2]int{start, i}, false, true
	case b[i] == '@' && i > start:
		return [2]int{start, i}, true, false
	}
	return [2]int{start, i}, false, false
}

// File is a log's file that In redacts in place: what it reads, and writes "*" over.
type File interface {
	io.ReaderAt
	io.WriterAt
}

// In overwrites each proxy key and URL password in the log f, size bytes long, with as many "*",
// in place, in what it gained since at: the file keeps its length and every other byte, so a
// program that is still writing to it at its own offset (Lima's hostagent, which opened it
// without O_APPEND) goes on where it was. It reads from the start of the line not yet ended at
// the last look, but no earlier than MatchMax before where that look stopped, so a long line that
// never ends is read once, not at every look, and reads at most limit bytes. It returns the look
// to start from next. A secret never spans a line, and only a line with an "@" can hold one. A
// look past the log's end, as after the log was emptied and started again, starts at 0.
func In(f File, size int64, at Look, limit int64) (Look, error) {
	if at.Line > size || at.Read > size {
		at = Look{}
	}
	from := max(at.Line, at.Read-MatchMax)
	b := make([]byte, min(size-from, limit))
	n, err := f.ReadAt(b, from)
	b = b[:n]
	if errors.Is(err, io.EOF) {
		err = nil
	}
	found := lineSpans(b)
	// What's read starts inside a line, past a secret's start that a look no longer reads, or
	// after one a rotation cut: the rest of a secret there is starred once its "@" comes.
	cut := at.Cut || from > at.Line
	open := false
	if cut {
		span, ended, more := leading(b)
		if ended {
			found = append(found, span)
		}
		open = more
	}
	if err == nil {
		err = overwrite(offsetWriter{f, from}, found)
	}
	if err != nil {
		return at, err
	}
	next := Look{Line: at.Line, Read: from + int64(len(b)), Cut: at.Cut && open}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		next.Line, next.Cut = from+int64(i)+1, false
	}
	return next, nil
}

// unendedKey and unendedPassword are the starts of a secret that a line cut before its end can
// hold: a proxy key without its "@" yet, and a URL's password without its "@" yet.
var (
	unendedKey      = regexp.MustCompile(`devvm:([0-9a-f]{1,64})$`)
	unendedPassword = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@"'` + "`" + `]*:([^\s@/?#]+)$`)
)

// Unended overwrites in b, a log's part whose last line may be cut before its end (a rotation's
// copy), the start of a secret at that end, which the patterns can't tell is one until its "@"
// comes: a proxy key's digits, and what follows a URL's user. A company proxy's address without
// a scheme can't be told from other text until its port, and stays. It reports whether it masked
// one, whose rest the log's writer goes on with (Look.Cut).
func Unended(b []byte) (masked bool) {
	if len(b) == 0 || b[len(b)-1] == '\n' {
		return false
	}
	last := b[bytes.LastIndexByte(b, '\n')+1:]
	for _, re := range []*regexp.Regexp{unendedKey, unendedPassword} {
		if m := re.FindSubmatchIndex(last); m != nil {
			copy(last[m[2]:m[3]], bytes.Repeat([]byte{'*'}, m[3]-m[2]))
			masked = true
		}
	}
	return masked
}

// Bytes is a file in memory, whose part In can redact as it does a log's.
type Bytes []byte

// ReadAt reads b from off.
func (b Bytes) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt writes over b at off, never past its end.
func (b Bytes) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > int64(len(b)) {
		return 0, io.ErrShortWrite
	}
	return copy(b[off:], p), nil
}

// lineSpans are the offsets of each secret in b, a line at a time, in the lines with an "@" alone.
func lineSpans(b []byte) [][2]int {
	var found [][2]int
	for start := 0; start < len(b); {
		end := bytes.IndexByte(b[start:], '\n') + 1
		if end == 0 {
			end = len(b) - start
		}
		if line := b[start : start+end]; bytes.IndexByte(line, '@') >= 0 {
			for _, s := range spans(line) {
				found = append(found, [2]int{start + s[0], start + s[1]})
			}
		}
		start += end
	}
	return found
}

// offsetWriter writes at its offsets plus off.
type offsetWriter struct {
	w   io.WriterAt
	off int64
}

func (o offsetWriter) WriteAt(p []byte, at int64) (int, error) { return o.w.WriteAt(p, o.off+at) }

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
