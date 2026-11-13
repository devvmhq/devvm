package netpolicy

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
)

// Log is a VM's event log, as devvm network log shows it. NewLog sets every
// field; tests change them.
type Log struct {
	Path string
	// Tick is how often a followed log is read for new events.
	Tick time.Duration
}

// NewLog returns vm's event log (paths.VM.EventLog).
func NewLog(d paths.Dirs, vm string) *Log {
	return &Log{Path: d.VM(vm).EventLog(), Tick: 250 * time.Millisecond}
}

// Old is the log's previous file, which the proxy renamed it to when it
// rotated it: its older events.
func (l *Log) Old() string { return l.Path + ".1" }

// Write writes the log's last n events to w, one line each (FormatEvent),
// the previous file's (Old) first. With follow it then writes each new
// one, until ctx ends, and goes on in the new log when the proxy rotates
// it. Lines that aren't events are skipped.
func (l *Log) Write(ctx context.Context, w io.Writer, n int, follow bool) error {
	f, err := os.Open(l.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var last []string
	keep := func(l string) {
		if s, ok := FormatEvent(l); ok && n > 0 {
			if len(last) == n {
				last = last[1:]
			}
			last = append(last, s)
		}
	}
	// The log is open first, so a rotation now leaves it as Old: read once.
	if err := l.readOld(f, keep); err != nil {
		return err
	}
	r := bufio.NewReader(f)
	var read int64
	partial, err := readLines(r, &read, keep)
	if err != nil {
		return err
	}
	if !follow {
		keep(partial)
	}
	for _, s := range last {
		if _, err := fmt.Fprintln(w, s); err != nil {
			return err
		}
	}
	if !follow {
		return nil
	}
	fl := follower{l: l, f: f, r: r, read: read, partial: partial, w: w}
	defer func() { _ = fl.f.Close() }()
	t := time.NewTicker(l.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := fl.next(); err != nil {
			return err
		}
	}
}

// Event is one event of a VM's network log, as its proxy wrote it.
type Event = proxy.Event

// Events returns the log's last n events, the previous file's (Old)
// first. Lines that aren't events are skipped.
func (l *Log) Events(n int) ([]Event, error) {
	var last []Event
	err := l.each(func(line string) {
		var e Event
		if json.Unmarshal([]byte(line), &e) != nil || e.Event == "" || e.Event == learnEvent || n <= 0 {
			return
		}
		if len(last) == n {
			last = last[1:]
		}
		last = append(last, e)
	})
	return last, err
}

// each calls line for each line of the log, the previous file's (Old)
// first, and for what follows its last newline.
func (l *Log) each(line func(string)) error {
	f, err := os.Open(l.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := l.readOld(f, line); err != nil {
		return err
	}
	var read int64
	rest, err := readLines(bufio.NewReader(f), &read, line)
	if err != nil {
		return err
	}
	line(rest)
	return nil
}

// readOld calls line for each line of Old, unless it's cur, the log
// already open.
func (l *Log) readOld(cur *os.File, line func(string)) error {
	old, err := os.Open(l.Old())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = old.Close() }()
	if a, err := old.Stat(); err == nil {
		if b, err := cur.Stat(); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	var read int64
	rest, err := readLines(bufio.NewReader(old), &read, line)
	if err != nil {
		return err
	}
	line(rest)
	return nil
}

// follower writes each new event of a followed log.
type follower struct {
	l       *Log
	f       *os.File
	r       *bufio.Reader
	read    int64
	partial string // a line still being written
	w       io.Writer
}

// next writes the events written since the last call. A log made shorter
// (emptied) is read again from its start. A log the proxy rotated is read
// to its end, then the new one from its start.
func (fl *follower) next() error {
	if fi, err := fl.f.Stat(); err == nil && fi.Size() < fl.read {
		if _, err := fl.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		fl.r.Reset(fl.f)
		fl.read, fl.partial = 0, ""
	}
	// Whether the log was renamed, checked before its end is read, so no
	// line written to it after is missed.
	nf := fl.rotated()
	if err := fl.drain(); err != nil {
		if nf != nil {
			_ = nf.Close()
		}
		return err
	}
	if nf == nil {
		return nil
	}
	_ = fl.f.Close()
	fl.f, fl.read, fl.partial = nf, 0, ""
	fl.r.Reset(nf)
	return fl.drain()
}

// rotated returns the log's new file, open, when the proxy has renamed
// the one being read.
func (fl *follower) rotated() *os.File {
	cur, err := fl.f.Stat()
	if err != nil {
		return nil
	}
	nf, err := os.Open(fl.l.Path)
	if err != nil {
		return nil
	}
	if fi, err := nf.Stat(); err != nil || os.SameFile(cur, fi) {
		_ = nf.Close()
		return nil
	}
	return nf
}

// drain writes the whole lines the log has after what was read.
func (fl *follower) drain() error {
	var werr error
	rest, err := readLines(fl.r, &fl.read, func(l string) {
		if s, ok := FormatEvent(fl.partial + l); ok && werr == nil {
			_, werr = fmt.Fprintln(fl.w, s)
		}
		fl.partial = ""
	})
	if err != nil {
		return err
	}
	if werr != nil {
		return werr
	}
	fl.partial += rest
	return nil
}

// readLines calls line for each whole line r has (without its newline),
// adding the bytes read to *read, and returns what's after the last
// newline: a line still being written.
func readLines(r *bufio.Reader, read *int64, line func(string)) (string, error) {
	for {
		s, err := r.ReadString('\n')
		*read += int64(len(s))
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		if err != nil {
			return "", err
		}
		line(strings.TrimSuffix(s, "\n"))
	}
}

// FormatEvent turns one line of an event log into what devvm network log shows:
// the time, allowed or BLOCKED, the host and port, and the rule that
// allowed it, the request, the tokens put in, or why it was blocked, after
// a blocked request's method and path. The
// VM chose the host and path: control characters are dropped. false for a
// line that isn't an event, and for devvm's mark of a VM starting to learn
// (MarkLearning), which is no connection.
func FormatEvent(line string) (string, bool) {
	var e proxy.Event
	if json.Unmarshal([]byte(line), &e) != nil || e.Event == "" || e.Event == learnEvent {
		return "", false
	}
	what, d := "allowed", "rule "+e.Rule
	switch e.Event {
	case "blocked":
		what, d = "BLOCKED", e.Reason
		if e.Method != "" && e.Method != http.MethodConnect && e.Path != "" { // a request, not a tunnel
			d = e.Method + " " + e.Path + ": " + e.Reason
		}
		if e.Quiet { // a host devvm leaves out on purpose, such as a tool's telemetry
			d += ", quiet"
		}
	case "forward":
		d = e.Method + " " + e.Path
		if e.Rule != "" { // the rule that let it through, by method and path
			d += ", rule " + e.Rule
		}
	case "intercept":
		d += ", decrypted"
	case "login": // an agent's login, captured at its token endpoint
		what, d = "login", e.Login+"'s login saved: "+e.Method+" "+e.Path
	case "nix-cache": // the shared Nix cache: a hit, fetched, or why not
		what, d = "cache", e.Method+" "+e.Path+": "+e.Reason
	case "link", "link_refused", "link_target_stopped", "link_limit": // devvm _links's
		what, d = linkEvent(e)
	}
	if len(e.Secrets) > 0 {
		d += ", token: " + strings.Join(e.Secrets, ",")
	}
	if e.Upstream != "" {
		d += ", through " + e.Upstream
	}
	if e.Via == proxy.ViaDNS {
		d += ", without proxy settings"
	}
	return fmt.Sprintf("%s  %-7s  %s  %s", msg.OneLine(e.Time), what, hostPort(e.Host, e.Port), msg.OneLine(d)), true
}

// linkEvent is what devvm network log shows of a link's event, whose host
// is the link's name: carried to its target, or refused and why.
func linkEvent(e proxy.Event) (what, d string) {
	switch e.Event {
	case "link":
		return "link", "to " + e.Target
	case "link_target_stopped":
		return "REFUSED", "to " + e.Target + ": it isn't running"
	case "link_limit":
		return "REFUSED", e.Reason + " connections carried already"
	}
	return "REFUSED", "to " + e.Target + ": " + e.Reason
}

// AppendEvent adds an event to a VM's event log (EventLog), the user's
// alone, with the time and the VM's name: devvm _links's, beside its
// proxy's. It's written through the proxy's own writer, which rotates
// the log by the proxy's rule, so an open VM keeps two
// at most too, and the proxy goes on writing to the new log.
func AppendEvent(path, vm string, e Event) error {
	e.Time, e.VM = time.Now().UTC().Format(time.RFC3339), vm
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	w, err := proxy.OpenEventLog(path)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func hostPort(host string, port int) string {
	return net.JoinHostPort(msg.OneLine(host), strconv.Itoa(port))
}

// Block is a host and port a VM's proxy blocked, as devvm network log
// --blocked lists it.
type Block struct {
	Last  string // when it was last blocked
	Count int    // how many times
	Host  string
	Port  int
	// Allow is the rule devvm network allow takes to allow it; "" for a
	// request refused whatever the allowlist says, a host the policy
	// doesn't take, or a quiet one, which devvm doesn't suggest.
	Allow string
	// Quiet is a host a built-in policy marks quiet (proxy.Event.Quiet),
	// such as a tool's telemetry: devvm leaves it out on purpose.
	Quiet bool
}

// Blocks returns the log's blocks (Old's too), one per host and port, by
// when each was last blocked.
func (l *Log) Blocks() ([]Block, error) {
	type key struct {
		host    string
		port    int
		refused bool
		quiet   bool // a quiet block and a plain one of the same host are rows of their own
	}
	rows := map[key]*Block{}
	err := l.each(func(l string) {
		var e proxy.Event
		if json.Unmarshal([]byte(l), &e) != nil || e.Event != "blocked" {
			return
		}
		k := key{e.Host, e.Port, e.Refused, e.Quiet}
		r := rows[k]
		if r == nil {
			r = &Block{Host: e.Host, Port: e.Port, Quiet: e.Quiet}
			if rule, ok := proxy.AllowRule(e.Host, e.Port); ok && !e.Refused && !e.Quiet {
				r.Allow = rule
			}
			rows[k] = r
		}
		r.Count++
		r.Last = e.Time
	})
	if err != nil {
		return nil, err
	}
	sorted := make([]Block, 0, len(rows))
	for _, r := range rows {
		sorted = append(sorted, *r)
	}
	slices.SortFunc(sorted, func(a, b Block) int {
		return cmp.Or(cmp.Compare(a.Last, b.Last), cmp.Compare(hostPort(a.Host, a.Port), hostPort(b.Host, b.Port)))
	})
	return sorted, nil
}

// WriteBlocked writes the log's blocks (Blocks) to w as a table, one row
// per host and port, by when it was last blocked: the last time, how many
// times, and the devvm network allow command that allows it. A request
// refused whatever the allowlist says, or a host the policy doesn't take,
// gets - instead: the command is meant to be pasted into a shell. The VM
// chose the host: control characters are dropped.
func (l *Log) WriteBlocked(w io.Writer) error {
	blocks, err := l.Blocks()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "LAST\tCOUNT\tHOST\tALLOW WITH")
	for _, b := range blocks {
		allow := "-"
		switch {
		case b.Allow != "":
			allow = "devvm network allow " + b.Allow
		case b.Quiet:
			allow = "- (quiet)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", msg.OneLine(b.Last), b.Count, hostPort(b.Host, b.Port), allow)
	}
	return tw.Flush()
}
