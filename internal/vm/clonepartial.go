package vm

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/proxy"
)

// A partial clone's clone in the VM lacks what the repo lacks, and the
// VM's git fetches each missing object from the repo's promisor remote
// when a command needs it, through the VM's network policy: the copy
// says when no promisor remote goes, or the policy blocks one's host
// (sayPartial). The VM's upload-pack never fetches an object it lacks to
// send it, so devvm's own fetch from the VM says what to do when that's
// why it failed (fetchInto).

// lockedWriter is w, written by one writer at a time, as two commands'
// errors may be.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write writes p to w, alone.
func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// packList is git rev-list, whose object list git pack-objects reads
// for a partial clone's pack (packCmd), run with a context of its own
// (cancel), so it can be stopped when pack-objects stops first. The two
// talk through a pipe of their own (pr, pw), whose ends this process
// closes once both have started (started), so when pack-objects ends
// early, rev-list's next write fails and it ends too, never waiting on a
// reader that's gone.
type packList struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	pr, pw *os.File
	closed sync.Once
	mu     sync.Mutex
	waited bool
	err    error
}

// started closes this process's ends of the pipe, once, which it never
// reads or writes: after both commands started, or either failed to.
func (l *packList) started() {
	l.closed.Do(func() {
		_ = l.pr.Close()
		_ = l.pw.Close()
	})
}

// wait waits for rev-list, once, and returns its error.
func (l *packList) wait() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.waited {
		l.waited = true
		l.err = l.cmd.Wait()
	}
	return l.err
}

// finish ends rev-list, stopped when the pack's end (packTail) never
// came, or never started, and returns its error, but for the stop.
func (l *packList) finish() error {
	defer l.cancel()
	l.started()
	l.mu.Lock()
	waited := l.waited
	l.mu.Unlock()
	switch {
	case l.cmd.Process == nil:
		return nil
	case !waited:
		l.cancel()
		_ = l.wait()
		return nil
	}
	return l.wait()
}

// packHold is how much of a pack's end packTail holds back: more than its
// trailing hash, SHA-256's 32 bytes, so a pack cut there is one git's
// index-pack refuses.
const packHold = 64

// packTail is a partial clone's pack, as the VM reads it: it holds back
// the pack's last packHold bytes until the pack ends, then waits for
// rev-list (check), and when rev-list failed, returns its error in place
// of those bytes, so the VM gets a pack cut short, which its index-pack
// refuses, never one that lacks objects without saying so.
type packTail struct {
	r     io.Reader
	check func() error
	buf   []byte
	held  []byte
	eof   bool
}

// Read reads the pack, all but its held-back end until it's all read.
func (t *packTail) Read(p []byte) (int, error) {
	for !t.eof && len(t.held) <= packHold {
		if t.buf == nil {
			t.buf = make([]byte, 32<<10)
		}
		n, err := t.r.Read(t.buf)
		t.held = append(t.held, t.buf[:n]...)
		switch {
		case errors.Is(err, io.EOF):
			t.eof = true
			if cerr := t.check(); cerr != nil {
				t.held = nil // never sent, after the error either
				return 0, cerr
			}
		case err != nil:
			return 0, err
		}
	}
	avail := len(t.held)
	if !t.eof {
		avail -= packHold
	}
	if avail == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.held[:avail])
	t.held = t.held[n:]
	return n, nil
}

// promisors are the remotes, with their first URL, that config sets as
// a partial clone's promisors: remote.<name>.promisor, its last value,
// true.
func promisors(config seedConfig) map[string]string {
	urls, promisor := map[string]string{}, map[string]bool{}
	for i := 0; i+1 < len(config.pairs); i += 2 {
		section, sub, name, ok := configKey(config.pairs[i])
		switch {
		case !ok || section != "remote":
		case name == "url" && urls[sub] == "":
			urls[sub] = config.pairs[i+1]
		case name == "promisor":
			promisor[sub] = config.pairs[i+1] == "true"
		}
	}
	out := map[string]string{}
	for sub, on := range promisor {
		if on && urls[sub] != "" {
			out[sub] = urls[sub]
		}
	}
	return out
}

// hostPromisors are the promisor remotes of the repo at dir as git here
// reads them, includes followed: those whose remote.<name>.promisor's
// last value is true as git reads it (gitBool, a key with no value
// true), each read alone, so a value git can't read drops only its own,
// and extensions.partialClone's; sorted, each once.
func hostPromisors(ctx context.Context, dir string) []string {
	var names []string
	if v, err := hostGit(ctx, dir, "config", "--get", "extensions.partialClone"); err == nil && v != "" {
		names = append(names, v)
	}
	out, _ := hostgit.Output(ctx, "-C", dir, "config", "--null", "--get-regexp", `^remote\..*\.promisor$`)
	on := map[string]bool{}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		key, value, hasValue := strings.Cut(rec, "\n")
		if section, sub, _, ok := configKey(key); ok && section == "remote" {
			on[sub] = !hasValue || gitBool(value)
		}
	}
	for sub, yes := range on {
		if yes {
			names = append(names, sub)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// keptPromisors are the promisor remotes of the repo at dir whose
// settings s's config doesn't send.
func (s seedData) keptPromisors(ctx context.Context, dir string) []string {
	sent := promisors(s.config)
	var kept []string
	for _, name := range hostPromisors(ctx, dir) {
		if _, ok := sent[name]; !ok {
			kept = append(kept, name)
		}
	}
	return kept
}

// urlHostPort is the host a remote's URL reaches and its port, a
// scheme's own unless the URL names one, or local for a file:// URL or a
// path, which reach this machine; host is "" for a URL it can't read.
func urlHostPort(u string) (host string, port int, local bool) {
	scheme, _, hasScheme := strings.Cut(u, "://")
	if !hasScheme {
		if h, _, ok := scpHost(u); ok {
			return strings.ToLower(h), 22, false
		}
		return "", 0, true
	}
	if strings.EqualFold(scheme, "file") {
		return "", 0, true
	}
	p, err := url.Parse(u)
	if err != nil || p.Hostname() == "" {
		return "", 0, false
	}
	port = map[string]int{"http": 80, "https": 443, "ssh": 22, "git+ssh": 22, "ssh+git": 22}[strings.ToLower(scheme)]
	if p.Port() != "" {
		n, err := strconv.Atoi(p.Port())
		if err != nil {
			return "", 0, false
		}
		port = n
	}
	if port == 0 {
		return "", 0, false
	}
	return strings.ToLower(p.Hostname()), port, false
}

// privateHost is whether host is one the internet tier never reaches:
// an address that isn't public (proxy.Public), or localhost. A name that
// resolves to such an address isn't reached either, which it can't tell.
func privateHost(host string) bool {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return !proxy.Public(a)
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// reach is what a VM's policies say of a host's port: whether its
// allowlist allows it, and whether a deny rule blocks it.
type reach struct{ allows, denies func(string, int) bool }

// sayPartial names, for a partial clone's seed into t's VM (data), what
// keeps git in the VM from the objects its clone lacks (partialWarnings).
func (e seeder) sayPartial(t *target, dir string, data seedData) {
	if !data.partial {
		return
	}
	// Policies it can't read judge nothing (partialWarnings).
	var r reach
	if al, err := e.Proxy.Allowlist(t.st); err == nil {
		r = reach{allows: al.Allows, denies: al.Denies}
	}
	for _, m := range partialWarnings(dir, data, netpolicy.TierOf(t.st), r) {
		e.Warn(m)
	}
}

// partialWarnings are sayPartial's messages for the seed of the repo at
// dir (data) into a VM of tier (r): the refs left out, the promisor
// remotes that don't go, and, for each that goes, in order, one on this
// machine, or whose host the tier blocks: none blocks every host, an
// allowlist those it doesn't allow, and internet those a deny rule
// blocks and private ones; with no policies to read (r's funcs nil), only
// the none tier's and private hosts. A host blocked on a port other than
// 80 or 443, such as ssh's, is named as the rule that allows that port
// alone (proxy.ExactRule), which devvm network allow takes.
func partialWarnings(dir string, data seedData, tier netpolicy.Tier, r reach) []msg.M {
	if !data.partial {
		return nil
	}
	var out []msg.M
	if m := data.refsMissing; len(m) > 0 {
		out = append(out, msg.ClonePartialRefsKept(len(m), dir, shown(m)))
	}
	if k := data.promisorsKept; len(k) > 0 {
		out = append(out, msg.ClonePartialPromisorKept(len(k), dir, shown(k)))
	}
	p := promisors(data.config)
	for _, name := range slices.Sorted(maps.Keys(p)) {
		host, port, local := urlHostPort(p[name])
		rule := host
		if exact, ok := proxy.ExactRule(host, port); ok && port != 80 && port != 443 {
			rule = exact
		}
		switch {
		case local:
			out = append(out, msg.ClonePartialUnreachable(dir, name, p[name]))
		case host == "":
		case tier == netpolicy.TierNone:
			out = append(out, msg.ClonePartialBlockedNone(dir, rule, name))
		case tier == netpolicy.TierAllowlist && r.allows != nil && !r.allows(host, port):
			out = append(out, msg.ClonePartialBlocked(dir, rule, name))
		case tier == netpolicy.TierInternet && (privateHost(host) || r.denies != nil && r.denies(host, port)):
			out = append(out, msg.ClonePartialDenied(dir, host, name))
		}
	}
	return out
}
