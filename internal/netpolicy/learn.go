package netpolicy

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// Learned is a host and port a learning VM reached that its allowlist
// tier doesn't allow, as devvm network lock offers it.
type Learned struct {
	Rule  string // the rule devvm network lock adds for it (proxy.ExactRule)
	Host  string
	Port  int
	Count int    // the connections and requests its proxy allowed
	Last  string // when it was last reached, as the log has it
	Agent string // the tool its requests named most (agentWord), "" for none
	// Tickable is whether it's one of the reviewed registries' and
	// vendors' hosts, on port 443 (netpolicy's Tickable).
	Tickable bool
	Risk     Risk
	// NoProxy is whether only tools without proxy settings reached it,
	// which the VM's names sent to its proxy (proxy.ViaDNS): devvm network
	// lock lists such hosts apart.
	NoProxy bool
}

// LearnedHosts is what a learning VM reached, since it started learning,
// that its allowlist tier doesn't allow: the tickable hosts first, then
// the others, then those only tools without proxy settings reached, then
// the risky ones, each by its rule.
type LearnedHosts struct {
	Since time.Time
	Hosts []Learned
	// Partial is whether the log lost events since then: the proxy
	// rotated it past its limit, keeping one older file, and the mark of
	// the VM starting to learn is gone with the file before.
	Partial bool
}

// Judge is how Learned judges a host and port, by the VM's policies on
// the allowlist tier: whether they allow it already, whether they deny
// it, so no allow would reach it, and whether a built-in policy marks it
// quiet, as a host devvm leaves out on purpose. None makes a host
// tickable, and Learned offers none of them.
type Judge struct {
	Allows, Denies, Quiet func(host string, port int) bool
}

// Learned returns what st's VM reached since it started learning that its
// allowlist tier wouldn't allow, by its proxy's own rules (the VM's
// policies on that tier, proxy.ReadAllowlist): none for a VM that doesn't
// learn.
func (p *Proxy) Learned(st *state.VM) (LearnedHosts, error) {
	lr := LearningOf(st)
	if !lr.On() {
		return LearnedHosts{}, nil
	}
	al, err := p.Allowlist(st)
	if err != nil {
		return LearnedHosts{}, err
	}
	return NewLog(p.Dirs, st.Name()).Learned(lr.Since, Judge{Allows: al.Allows, Denies: al.Denies, Quiet: al.Quiet})
}

// Allowlist is the policies st's VM has on the allowlist tier (Policies), read as its proxy reads
// them (proxy.ReadAllowlist).
func (p *Proxy) Allowlist(st *state.VM) (proxy.Allowlist, error) {
	set := Policies(p.Dirs, st.Name(), TierAllowlist, image.Name(st.Image), tools.Names(st))
	return proxy.ReadAllowlist(p.Assets, set.Builtin, set.Files, set.Denies)
}

// reached is whether an event is a connection or request the proxy let
// through to its host: a tunnel, a decrypted one, or a forwarded request.
func reached(event string) bool {
	return event == "tunnel" || event == "intercept" || event == "forward"
}

// learnEvent is the event devvm writes in a VM's log as the VM starts
// learning (MarkLearning), so Learned knows whether the log still goes
// back that far. devvm network log doesn't show it.
const learnEvent = "learn"

// MarkLearning writes in the log that its VM, vm, starts learning now,
// through the proxy's own writer (AppendEvent).
func (l *Log) MarkLearning(vm string) error { return AppendEvent(l.Path, vm, Event{Event: learnEvent}) }

// learnedEvent is what Learned reads of an event: only these.
type learnedEvent struct {
	Time, Event, Host, Agent, Via string
	Port                          int
}

// readEvent reads what Learned needs of a line of the log; false for a
// line that isn't an event.
func readEvent(line string) (learnedEvent, bool) {
	var e learnedEvent
	return e, json.Unmarshal([]byte(line), &e) == nil && e.Event != ""
}

// countMax is the most of a VM's log a launch's reminder reads to count
// the hosts the VM learned, both files: past it the reminder doesn't
// count, so a launch never parses all 20 MiB a log can hold.
const countMax = 2 << 20

// Countable reports whether the log is small enough for a launch's
// reminder to count the hosts its VM learned (countMax).
func (l *Log) Countable() bool {
	var n int64
	for _, p := range []string{l.Path, l.Old()} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n <= countMax
}

// learning is what Learned has counted so far: a row for each rule, the
// tools that reached each, the rules reached with proxy settings, and the
// rules it left out, each judged once.
type learning struct {
	j       Judge
	rows    map[reachedKey]*Learned
	agents  map[reachedKey]map[string]int
	proxied map[reachedKey]bool
	skipped map[reachedKey]bool
}

// add counts e, a host reached, unless it's left out: a host the policy
// doesn't take, an address that isn't public, or a host the allowlist
// allows already or denies.
func (c *learning) add(e learnedEvent) {
	k := reachedKey{e.Host, e.Port}
	if c.skipped[k] {
		return
	}
	r := c.rows[k]
	if r == nil {
		rule, ok := proxy.ExactRule(e.Host, e.Port)
		a, err := netip.ParseAddr(strings.Trim(e.Host, "[]"))
		if !ok || err == nil && !proxy.Public(a) || c.j.Allows(e.Host, e.Port) || c.j.Denies(e.Host, e.Port) ||
			c.j.Quiet != nil && c.j.Quiet(e.Host, e.Port) {
			c.skipped[k] = true
			return
		}
		r = &Learned{Rule: rule, Host: e.Host, Port: e.Port, Tickable: Tickable(e.Host, e.Port), Risk: RiskOf(e.Host)}
		c.rows[k], c.agents[k] = r, map[string]int{}
	}
	r.Count++
	r.Last = e.Time
	if e.Via != proxy.ViaDNS {
		c.proxied[k] = true
	}
	if a := agentWord(e.Agent); a != "" {
		c.agents[k][a]++
	}
}

// reachedKey is a host and port as the log has them, which Learned counts
// by.
type reachedKey struct {
	host string
	port int
}

// after reports whether at, an event's time, isn't before from, given as
// RFC 3339 in UTC (fromText too): compared as text when at is in the same
// form, as the proxy writes it, which is the same order and much quicker
// than parsing every line's.
func after(at, fromText string, from time.Time) bool {
	if len(at) == len(fromText) && strings.HasSuffix(at, "Z") {
		return at >= fromText
	}
	t, err := time.Parse(time.RFC3339, at)
	return err == nil && !t.Before(from)
}

// Learned returns the hosts and ports the log's VM reached since then
// (the previous file's too) that j doesn't allow or deny, one per rule,
// each judged once: an address that isn't public is left out, as is a
// host the policy doesn't take. A log that isn't there has none; one
// without the mark of the VM starting to learn then (MarkLearning), which
// the proxy's rotation dropped, is partial.
func (l *Log) Learned(since time.Time, j Judge) (LearnedHosts, error) {
	out := LearnedHosts{Since: since}
	from := since.Truncate(time.Second)
	fromText := from.UTC().Format(time.RFC3339)
	c := learning{j: j, rows: map[reachedKey]*Learned{}, agents: map[reachedKey]map[string]int{}, proxied: map[reachedKey]bool{}, skipped: map[reachedKey]bool{}}
	marked := false
	err := l.each(func(line string) {
		e, ok := readEvent(line)
		if !ok {
			return
		}
		switch {
		case !after(e.Time, fromText, from):
		case e.Event == learnEvent:
			marked = true
		case reached(e.Event):
			c.add(e)
		}
	})
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, err
	}
	out.Partial = !marked
	for k, r := range c.rows {
		r.Agent, r.NoProxy = mostOf(c.agents[k]), !c.proxied[k]
		out.Hosts = append(out.Hosts, *r)
	}
	slices.SortFunc(out.Hosts, func(a, b Learned) int { return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Rule, b.Rule)) })
	return out, nil
}

// rank is where a learned host goes in the list: the tickable ones
// first, then the others, then those only tools without proxy settings
// reached, then the risky ones.
func rank(l Learned) int {
	switch {
	case l.Risk != "":
		return 3
	case l.NoProxy:
		return 2
	case l.Tickable:
		return 0
	}
	return 1
}

// Ticked is whether the lock's list starts with h ticked: tickable, and
// not risky.
func (h Learned) Ticked() bool { return h.Tickable && h.Risk == "" }

// agentMax bounds the tool a learned host shows.
const agentMax = 32

// agentWord is the tool a User-Agent names: its first word, such as
// npm/10.9.2 or curl/8.5.0, when it's at most agentMax characters, each
// a letter, a digit or one of ._/+-; else "". The VM chose it, so nothing
// else is shown, such as characters that look like others or take no
// room.
func agentWord(ua string) string {
	w, _, _ := strings.Cut(strings.TrimLeft(ua, " \t"), " ")
	if w == "" || len(w) > agentMax || strings.ContainsFunc(w, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._/+-", c)
	}) {
		return ""
	}
	return w
}

// mostOf is the key counted most, the first by name of those counted as
// often; "" for none.
func mostOf(counts map[string]int) string {
	best, n := "", 0
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if counts[k] > n {
			best, n = k, counts[k]
		}
	}
	return best
}
