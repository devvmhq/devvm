package netpolicy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// Rule is the rule for devvm network allow (deny false) or deny: rule as
// given, with deny's leading ! added, narrowed to methods and the path
// pattern p when --method and --path give them ("" or secrets.None for
// none), in the proxy's canonical form when it narrows. It refuses what
// the proxy wouldn't take as one rule, a rule with spaces in it, which the
// flags write, an allow of a deny, and methods or a path on an address, a
// CIDR or **.
func Rule(rule string, deny bool, methods, p string) (string, error) {
	if deny && !strings.HasPrefix(rule, "!") {
		rule = "!" + rule
	}
	if !proxy.ValidRule(rule) || strings.ContainsAny(rule, " \t") || !deny && strings.HasPrefix(rule, "!") {
		return "", msg.NotARule(rule)
	}
	if methods == secrets.None {
		methods = ""
	}
	if p == secrets.None {
		p = ""
	}
	if methods == "" && p == "" {
		return rule, nil
	}
	if !proxy.Narrowable(rule) {
		return "", msg.RuleNotNarrowable(rule)
	}
	if methods != "" {
		var err error
		if methods, err = secrets.CheckMethods(methods); err != nil {
			return "", err
		}
	}
	if p != "" {
		if err := secrets.CheckPath(p); err != nil {
			return "", err
		}
	}
	narrowed, _ := proxy.NarrowRule(rule, methods, p)
	return narrowed, nil
}

// NarrowedHost is a rule's host as written, without its !, port, methods
// or path, when the rule narrows it to some requests; false for any other
// rule.
func NarrowedHost(rule string) (string, bool) {
	f := strings.Fields(strings.TrimPrefix(rule, "!"))
	if len(f) < 2 {
		return "", false
	}
	h, _, err := net.SplitHostPort(f[0])
	if err != nil {
		h = f[0]
	}
	return h, true
}

// NarrowNotes says, once for each host that rules narrow, that its proxy
// now decrypts it to check each request (rule_narrows). For an allow whose
// host a rule that doesn't narrow already allows whole on st's VM's
// allowlist, or with st nil on each VM's, it warns instead, naming those
// VMs, that the allow narrows nothing there, or only port 80's plain HTTP,
// because allows add up, and names the deny that would (narrowsOn).
//
// It runs once the rules are written, so a VM whose rules can't be read is
// a warning naming it (rule_narrows_unknown), never a failure of the change.
func (p *Proxy) NarrowNotes(rules []string, st *state.VM, deny bool) {
	var als []namedAllowlist
	noted := map[string]bool{}
	for _, r := range rules {
		h, ok := NarrowedHost(r)
		if !ok {
			continue
		}
		decrypts := true
		if !deny {
			if als == nil {
				als = allowlists(p, st)
			}
			decrypts = narrowsOn(p, als, h, r)
		}
		if decrypts && !noted[h] {
			noted[h] = true
			p.Note(msg.RuleNarrows(h))
		}
	}
}

// namedAllowlist is a VM's allowlist (Proxy.Allowlist), by the VM's name.
type namedAllowlist struct {
	vm string
	al proxy.Allowlist
}

// allowlists are the allowlists an allow by method and path joins: st's
// VM's, or with st nil, each VM's, as -g's file joins every VM's. A VM's
// that can't be read is left out, with a warning naming it.
func allowlists(p *Proxy, st *state.VM) []namedAllowlist {
	sts := []*state.VM{st}
	if st == nil {
		sts = nil
		vms, _ := state.VMNames(p.Dirs)
		for _, vm := range vms {
			if s, err := state.ReadVM(p.Dirs, vm); err == nil && s.Exists() {
				sts = append(sts, s)
			}
		}
	}
	out := make([]namedAllowlist, 0, len(sts))
	for _, s := range sts {
		al, err := p.Allowlist(s)
		if err != nil {
			p.Warn(msg.RuleNarrowsUnknown(err.Error(), s.Name()).Because(err))
			continue
		}
		out = append(out, namedAllowlist{s.Name(), al})
	}
	return out
}

// narrowsOn warns about the VMs whose allowlists already allow the host h
// of r, an allow by method and path, whole (proxy.Allowlist.CoversWhole):
// where it narrows nothing, and where it narrows only port 80's plain HTTP.
// It reports whether r narrows HTTPS on any of them, or there's none, so
// the proxy decrypts the host.
func narrowsOn(p msg.Reporter, als []namedAllowlist, h, r string) bool {
	var nothing, httpOnly []string
	https := len(als) == 0
	for _, a := range als {
		switch no, plain := a.al.CoversWhole(r); {
		case no:
			nothing = append(nothing, a.vm)
		case plain:
			httpOnly = append(httpOnly, a.vm)
		default:
			https = true
		}
	}
	if len(nothing) > 0 {
		p.Warn(msg.RuleNarrowsNothing(h, r, strings.Join(nothing, ", ")))
	}
	if len(httpOnly) > 0 {
		p.Warn(msg.RuleNarrowsHTTPOnly(h, r, strings.Join(httpOnly, ", ")))
	}
	return https
}

// OtherBinaries warns, when rules hold one by method and path, about each
// VM whose proxy runs from another devvm binary (otherBinary), which may
// read that rule otherwise until the VM's next launch: vm's, or with none,
// every VM's.
func (p *Proxy) OtherBinaries(rules []string, vm string) {
	if !slices.ContainsFunc(rules, func(r string) bool { _, ok := NarrowedHost(r); return ok }) {
		return
	}
	vms := []string{vm}
	if vm == "" {
		vms, _ = state.VMNames(p.Dirs)
	}
	for _, v := range vms {
		if otherBinary(p, v) {
			p.Warn(msg.ProxyOtherBinary(v))
		}
	}
}

// sameRule reports whether the policy line text and rule are one rule: the
// same in the proxy's canonical form, or, for what isn't a rule, the same
// text in any case.
func sameRule(text, rule string) bool {
	a, okA := proxy.CanonicalRule(text)
	b, okB := proxy.CanonicalRule(strings.TrimSpace(rule))
	if okA && okB {
		return a == b
	}
	return strings.EqualFold(text, strings.TrimSpace(rule))
}

// readRules reads a policy file's lines; a missing file has none. It reads
// it as the proxy does (proxy.ReadPolicy), so a FIFO or a device there
// can't hang devvm network or doctor.
func readRules(path string) ([]string, error) {
	b, err := proxy.ReadPolicy(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil || len(b) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n"), nil
}

// writeRules replaces a policy file whole, so its VMs' proxies never read
// half of it, through a link if it is one.
func writeRules(path string, lines []string) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	return state.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// AddRule adds rule to the policy file at path, unless a line has it
// already (sameRule); false then.
func AddRule(path, rule string) (bool, error) {
	lines, err := readRules(path)
	if err != nil {
		return false, err
	}
	for _, l := range proxy.ParsePolicy(strings.Join(lines, "\n")) {
		if sameRule(l.Text, rule) {
			return false, nil
		}
	}
	return true, writeRules(path, append(lines, rule))
}

// RemoveRule removes the lines with rule (sameRule, comments aside) from
// the policy file at path; false when none has it.
func RemoveRule(path, rule string) (bool, error) {
	lines, err := readRules(path)
	if err != nil {
		return false, err
	}
	drop := map[int]bool{}
	for _, l := range proxy.ParsePolicy(strings.Join(lines, "\n")) {
		drop[l.N] = sameRule(l.Text, rule)
	}
	kept := lines[:0:0]
	for i, l := range lines {
		if !drop[i+1] {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(lines) {
		return false, nil
	}
	return true, writeRules(path, kept)
}

// RuleFile is a policy and its lines that hold more than a comment, each
// marked whether it's a rule (proxy.ParsePolicy): a built-in one, by its
// name in devvm's files, or one of the user's files, by its path.
type RuleFile struct {
	Path    string // the file's path, or the built-in policy's name
	Builtin bool
	// DeniesOnly is a file whose denies alone apply (PolicySet.Denies):
	// its Lines are its denies and its lines that aren't rules.
	DeniesOnly bool
	Lines      []proxy.PolicyLine
}

// Rules are the file's rules, as written, comments left out.
func (f RuleFile) Rules() []string {
	var out []string
	for _, l := range f.Lines {
		if l.OK {
			out = append(out, l.Text)
		}
	}
	return out
}

// NotRules are the file's lines that aren't rules, which the proxy skips:
// a built-in policy's quiet lines (Quiet) aside.
func (f RuleFile) NotRules() []proxy.PolicyLine {
	var out []proxy.PolicyLine
	for _, l := range f.Lines {
		if !l.OK && (!f.Builtin || !l.Quiet()) {
			out = append(out, l)
		}
	}
	return out
}

// Quiet are a built-in policy's quiet lines' hosts, as HOST[:PORT]: hosts
// it leaves out on purpose, blocked without a notice in the VM. None for a
// file of the user's, whose quiet lines the proxy skips (NotRules).
func (f RuleFile) Quiet() []string {
	var out []string
	for _, l := range f.Lines {
		if f.Builtin && l.Quiet() {
			out = append(out, strings.Fields(l.Text)[1])
		}
	}
	return out
}

// ReadRules returns each of the policies (Policies) that holds more than
// comments, in the order its proxy reads them, with its lines: the
// built-in ones from devvm's files, own, then the user's files from disk.
func ReadRules(own fs.FS, builtin, files []string) ([]RuleFile, error) {
	var out []RuleFile
	for _, n := range builtin {
		b, err := fs.ReadFile(own, n)
		if err != nil {
			return nil, err
		}
		if pl := proxy.ParsePolicy(string(b)); len(pl) > 0 {
			out = append(out, RuleFile{Path: n, Builtin: true, Lines: pl})
		}
	}
	for _, f := range files {
		rf, err := readPolicyFile(f, false)
		if err != nil {
			return nil, err
		}
		if len(rf.Lines) > 0 {
			out = append(out, rf)
		}
	}
	return out, nil
}

// ReadPolicies is ReadRules for a VM's policies (Policies): the built-in
// ones and its files, then the files whose denies alone apply, each with
// only its denies and its lines that aren't rules.
func ReadPolicies(own fs.FS, s PolicySet) ([]RuleFile, error) {
	out, err := ReadRules(own, s.Builtin, s.Files)
	if err != nil {
		return nil, err
	}
	for _, f := range s.Denies {
		rf, err := readPolicyFile(f, true)
		if err != nil {
			return nil, err
		}
		if len(rf.Lines) > 0 {
			out = append(out, rf)
		}
	}
	return out, nil
}

// readPolicyFile is the user's policy file f with its lines; with
// deniesOnly, those of them that are denies or aren't rules.
func readPolicyFile(f string, deniesOnly bool) (RuleFile, error) {
	lines, err := readRules(f)
	if err != nil {
		return RuleFile{}, err
	}
	pl := proxy.ParsePolicy(strings.Join(lines, "\n"))
	if deniesOnly {
		pl = slices.DeleteFunc(pl, func(l proxy.PolicyLine) bool { return l.OK && !strings.HasPrefix(l.Text, "!") })
	}
	return RuleFile{Path: f, DeniesOnly: deniesOnly, Lines: pl}, nil
}

// notRuleMark follows a line that isn't a rule in WriteRules' output.
const notRuleMark = "  # not a rule, skipped"

// quietMark follows a built-in policy's quiet line in WriteRules' output:
// a host it leaves out on purpose, blocked without telling the VM.
const quietMark = "  # blocked, without a notice in the VM"

// builtinMark labels a built-in policy in WriteRules' output, before its
// name: it comes with devvm, so it has no path.
const builtinMark = "devvm: "

// deniesOnlyMark follows the path of a file whose denies alone apply in
// WriteRules' output.
const deniesOnlyMark = " (denies only)"

// WriteRules writes each policy's rules to w: after #, a file's path or
// a built-in policy's name, labelled so, then its lines, indented, each
// that isn't a rule marked so, with its line number.
func WriteRules(w io.Writer, files []RuleFile) error {
	for _, f := range files {
		label := f.Path
		if f.Builtin {
			label = builtinMark + f.Path
		}
		if f.DeniesOnly {
			label += deniesOnlyMark
		}
		if _, err := fmt.Fprintf(w, "# %s\n", label); err != nil {
			return err
		}
		for _, l := range f.Lines {
			text, mark := l.Text, ""
			switch {
			case l.Quiet() && f.Builtin:
				mark = quietMark
			case !l.OK:
				// Anything but a rule may hold control characters: dropped.
				text, mark = msg.OneLine(l.Text), notRuleMark+" (line "+strconv.Itoa(l.N)+")"
			}
			if _, err := fmt.Fprintf(w, "  %s%s\n", text, mark); err != nil {
				return err
			}
		}
	}
	return nil
}

// NotRules returns doctor's warnings (PolicyWarnings) for the policy files
// you edit: every VM's, your layers' and each VM's own.
func NotRules(d paths.Dirs) []msg.M {
	files := []string{d.Policy()}
	layers, _ := filepath.Glob(filepath.Join(d.Layers(), "*.policy"))
	files = append(files, layers...)
	vms, _ := state.VMNames(d)
	for _, vm := range vms {
		files = append(files, d.VM(vm).Policy())
	}
	return PolicyWarnings(files)
}

// PolicyWarnings returns a warning for each line of files that isn't a
// rule: one the proxy skips, or a broken deny (proxy.BrokenDeny), which
// makes the proxies that read it deny everything. A file it can't read is
// left out.
func PolicyWarnings(files []string) []msg.M {
	var out []msg.M
	for _, f := range files {
		rs, err := ReadRules(nil, nil, []string{f})
		if err != nil {
			continue
		}
		for _, rf := range rs {
			for _, l := range rf.NotRules() {
				n := strconv.Itoa(l.N)
				switch {
				case proxy.BrokenDeny(l):
					out = append(out, msg.PolicyBrokenDeny(f, n, l.Text))
				case l.Quiet():
					out = append(out, msg.PolicyQuietNotOwn(f, n, l.Text))
				default:
					out = append(out, msg.PolicyNotRule(f, n, l.Text))
				}
			}
		}
	}
	return out
}
