//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
)

// linkSection: links between VMs, each check the end-to-end proof of a
// row of the design note's threat check (internals/design.md, "Links
// between VMs"). Three small VMs without network policy, lkdb and lkapi
// the targets, lkapp the client, then network policy on the client and
// the first target; ports $P47240-$P47249, and 80. devvm runs with a soft
// limit of 256 open files, macOS's default (a shell of yours may have
// more), so the link sessions it starts get it too.
func linkSection(s *S) {
	s.Section("links between VMs")
	var old syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old) == nil {
		low := old
		low.Cur = min(old.Cur, 256)
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low)
		defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) }()
	}
	l := &link{s: s.Needing(backend.FeatureLink), x: "lk"}
	l.w1, l.w2, l.n2, l.g = l.x+"db", l.x+"api", l.x+"api", l.x+"g"
	linkMake(l)
	linkChecks(l)
	linkHostile(l)
	linkTargets(l)
	linkGroups(l)
	linkPolicy(l)
	_, _ = s.Devvm(l.p1, "rm", "--force", "--no-export")
	_, _ = s.Devvm(l.p2, "rm", "--force", "--no-export")
}

// linkMake makes the three VMs and their servers.
func linkMake(l *link) {
	s := l.s
	l.p1, _ = s.NewProject(l.x + "db")
	l.p2, _ = s.NewProject(l.x + "api")
	l.pc, _ = s.NewProject(l.x + "app")
	small := []string{"--network", "open", "--no-git-protect", "--cpus", "2", "--memory", "2"}
	l.v1, _, _ = s.Launch(l.p1, "exit", append(small, "--ports", "none")...)
	l.v2, _, _ = s.Launch(l.p2, "exit", append(small, "--ports", "none")...)
	l.vc, _, _ = s.Launch(l.pc, "exit", append(small, "--ports", "all")...)
	s.Check("two targets that forward no port and a client that forwards every port, without network policy", func() error { return lkMade(l) })
	_ = lkServe(l, l.v1, l.w1, l.p(0), l.p(2), l.p(3), l.p(4), l.p(9), "80")
	_ = lkServe(l, l.v2, l.w2, l.p(0), "80")
	_ = lkPrep(l, l.vc)
	s.Check("sshd -T in every VM: remote forwards on 127.77.*:* alone, at the address asked, no socket forwards, tunnels or X11",
		func() error { return lkSshdT(s, l.v1, l.v2, l.vc) })
	s.Check("Lima's own rules ignore every port of the client's", func() error {
		r, err := s.Limactl("list", "--format", "{{range .Config.PortForwards}}{{.GuestIP}}={{.Ignore}} {{end}}", l.vc)
		if err != nil {
			return err
		}
		if got := out(r); got != "127.0.0.1=true 0.0.0.0=true ::=true " {
			return fmt.Errorf("Lima's rules: %q", got)
		}
		return nil
	})
}

// linkChecks: publish, add and rm on the running client, names, ports
// and what nothing listens on.
func linkChecks(l *link) {
	s := l.s
	s.Check("devvm link publish offers ports, and lists them, with \"version\": 1 in its --json", func() error { return lkPublish(l) })
	s.Check("a port the target doesn't publish is refused by devvm link add", func() error { return lkUnpublishedAdd(l) })
	s.Check("devvm link add on the running client applies within seconds: NAME:PORT and NAME.devvm.internal:PORT, by curl and a raw TCP client",
		func() error { return lkAddApplies(l) })
	s.Check("--as NAME names the link alone: the target's folder name doesn't resolve in the client", func() error {
		switch {
		case !lkResolves(s, l.vc, "db") || !lkResolves(s, l.vc, "db.devvm.internal"):
			return errors.New("db doesn't resolve in the client")
		case lkResolves(s, l.vc, l.x+"db") || lkResolves(s, l.vc, l.x+"db.devvm.internal"):
			return errors.New(l.x + "db resolves in the client")
		}
		return nil
	})
	s.Check("two targets publishing the same port are each reached by their own name", func() error { return lkTwo(l) })
	s.Check("a port below 1024 (80) is linked on the same port, sshd listening on it as the VM's user", func() error { return lkLow(l) })
	s.Check("devvm link lists each link, with its target, folder and status, and \"version\": 1 in its --json", func() error { return lkList(l) })
	s.Check("devvm network allowlist is refused for a client linked to a target without network policy", func() error { return lkOnRefused(l) })
	s.Check("a published port that isn't linked is refused: nothing listens for it in the client", func() error {
		a, err := lkRaw(s, l.vc, "db", l.p(4))
		Note(s, "db:%s: %s", l.p(4), orNothing(a))
		return not(err == nil, "db:"+l.p(4)+" took the connection")
	})
	s.Check("after devvm link unpublish, the link's next connection is refused and logged, and the clients are named", func() error { return lkUnpublish(l) })
	s.Check("devvm link rm on the running client applies within seconds: its listener goes", func() error { return lkRmApplies(l) })
	s.Check("nothing from a link listens on the client's 127.0.0.1, ::1 or every address; its sshd listens only on its own port and the links' addresses",
		func() error { return lkVMClean(s, l.vc, l.p(0), "80") })
	s.Check("the client forwards every port, and no link is forwarded to this machine", func() error {
		time.Sleep(Settle)
		switch {
		case !lkListening(s, l.vc, "127.77.0.1:"+l.p(0)):
			return errors.New("the link doesn't listen in the client")
		case Reaches(s, "127.0.0.1:"+l.p(0)) || Reaches(s, "[::1]:"+l.p(0)):
			return errors.New("the link's port is forwarded to this machine")
		}
		return nil
	})
	s.Check("a server of the client's own on its 127.0.0.1 with a link's port runs and reaches this machine as usual (the control); the link still reaches its target",
		func() error { return lkOwnServer(l) })
	s.Check("each link's end on this machine is a socket of mode 600 in a folder of mode 700, and no TCP port of this machine's carries it",
		func() error { return lkSockets(l) })
	s.Check("ssh -R into the client binds nothing on 0.0.0.0, 127.0.0.1, ::1, localhost, another range or a socket; on 127.77.0.0/16 it does (the control)",
		func() error { return RfwdLinks(s, l.vc, filepath.Join(s.sb.work, "ssh", "agent.sock"), lkSSH(s)...) })
	s.Check("a program in the client on a link's address and port first fails that link alone (link_port_taken); another link works", func() error { return lkPortTaken(l) })
	s.Check("256 connections at once, past the socket's backlog, all carried; a 257th is refused, and devvm network log shows link_limit",
		func() error { return lkCap(l) }, Flaky("256 connections opened at once, and their relays ending within a minute, depend on how busy this machine is"))
	s.Check("devvm link rm ends the link's open connections, though neither the client nor the target's server closes them: their ssh on this machine ends",
		func() error { return lkRmEnds(l) })
}

// linkHostile: root in the client loosening its sshd.
func linkHostile(l *link) {
	s := l.s
	s.Check("root in the client lets sshd bind any address: the link session still asks only for its links' addresses, and the link works", func() error { return lkHostileListen(l) })
	s.Check("... with GatewayPorts yes too: the link session still listens on no TCP port here and connects only to the VMs' sshd, and the link works", func() error { return lkHostileGateway(l) })
	s.r.out.line("  recorded: %s", l.recorded)
	s.Check("sshd put back, the session's next connection binds the links' addresses alone again", func() error {
		if err := lkHonest(s, l.vc); err != nil {
			return err
		}
		if err := lkReconnect(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
			return err
		}
		if err := s.Until(15*time.Second, func() error { return lkGets(s, l.vc, l.n2, l.p(0), l.w2+"-"+l.p(0)) }); err != nil {
			return err
		}
		if err := lkVMClean(s, l.vc, l.p(0)); err != nil {
			return err
		}
		return lkSshdT(s, l.vc)
	})
	s.Check("devvm network log shows the links' connections and refusals without network policy", func() error { return lkLogOff(l) })
}

// linkTargets: a stopped, restarted and removed target, and a VM made
// again in its folder.
func linkTargets(l *link) {
	s := l.s
	s.Check("a stopped target: the connection fails, devvm network log shows link_target_stopped, the launch warns, and the target stays stopped",
		func() error { return lkStopped(l) }, Flaky("the client's refusal reaches its event log within 5 s of the connection, which a busy machine can miss"))
	s.Check("restarted, it works again, with its new host key", func() error { return lkRestarted(l) })
	s.Check("devvm rm of the target removes the client's link, names the client, and its sockets go; the client's session runs on", func() error { return lkRmTarget(l) })
	s.Check("a VM made again in that folder isn't linked", func() error { return lkMadeAgain(l) })
}

// linkGroups: the three VMs, none with network policy, in one group.
func linkGroups(l *link) {
	s := l.s
	s.Check("three VMs join a group, the first making it, the trust said on each join: each reaches the ports the others publish by name", func() error { return gpJoin(l) })
	s.Check("devvm group lists the three, each with its name, folder and published ports, and \"version\": 1 in its --json", func() error { return gpShow(l) })
	s.Check("a port a member publishes reaches the others' links at once", func() error { return gpPublish(l) })
	s.Check("in a group: devvm network allowlist is refused, so is joining another group, and devvm link rm of the group's link", func() error { return gpRefused(l) })
	s.Check("devvm group leave removes only the group's links, in it and the others, naming them; the user's link stays; the last one's leaving removes the group", func() error { return gpLeave(l) })
	s.Check("a join is refused when a member's name is the joiner's own hostname, whatever name it joins as", func() error { return gpHostname(l) })
}

// linkPolicy: network policy on the client and the first target, the
// tier rule's refusals, a group's all-or-none and nothing left behind.
func linkPolicy(l *link) {
	s := l.s
	s.Check("devvm network allowlist for a target while it runs, then for the client, restarted behind its proxy", func() error { return lkPolicyOn(l) })
	s.Check("the target's network policy on but not yet enforced (devvm ls: unenforced): the link's next connection is refused and logged, and devvm link add and the client's launch warn",
		func() error { return lkPolicyUnenforced(l) })
	s.Check("the target restarted behind its proxy", func() error {
		r, err := lkUp(s, l.p1, "restart")
		if err != nil || s.Paths.State(l.v1)["net.made.check"] != "ok" {
			return errors.Join(errors.New(l.v1+": not restarted behind its proxy"), err, errorsOf(r))
		}
		return lkServe(l, l.v1, l.w1, l.p(0), "80")
	})
	s.Check("with network policy, the client reaches NAME:PORT and NAME.devvm.internal:PORT by curl through its proxy settings and a raw TCP client; a direct connection fails (the control)",
		func() error { return lkPolicyReach(l) })
	s.Check("a link added while the client with network policy runs: curl through its proxy reaches it at once, by GET and CONNECT, no link name is in NO_PROXY, and nothing restarts",
		func() error { return lkProxyAdded(l) })
	s.Check("the client's proxy reaches only the links on its own list: not a name or port it hasn't linked, a name doubled or an address; the target's proxy doesn't reach the client's links",
		func() error { return lkProxyOwn(l) })
	s.Check("devvm network log shows the links' connections with network policy", func() error { return lkLogOn(l) })
	s.Check("a client with network policy is refused by devvm link add for a target without it", func() error {
		r, err := s.DevvmNames(l.pc, "link", "add", l.n2, l.p(0))
		if err == nil {
			return errors.New("devvm link add went on")
		}
		if err := said(r, "devvm: link_tier_mismatch target="+l.v2+" target_tier=open tier=allowlist vm="+l.vc); err != nil {
			return err
		}
		return not(lkResolves(s, l.vc, l.n2), l.n2+" resolves in the client")
	})
	s.Check("devvm network open is refused for a target that a client with network policy links to", func() error {
		r, err := s.DevvmNames(l.p1, "network", "open")
		if err == nil {
			return errors.New("devvm network open went on")
		}
		if err := said(r, "devvm: tier_linked_clients tier=open vm="+l.v1+" vms="+l.vc); err != nil {
			return err
		}
		return not(s.Paths.State(l.v1)["net.tier"] != "allowlist", l.v1+" isn't on allowlist")
	})
	s.Check("the tier rule: an allowlist client's link to a target on none goes on, to one on internet is refused and warned of, and goes on once the target is back on allowlist",
		func() error { return lkTiers(l) })
	s.Check("the rule broken under devvm: the link's next connection is refused, and the client's launch warns and runs on without it", func() error { return lkPolicyBroken(l) })
	s.Check("the rule restored, the client's next launch has the link again", func() error { return lkPolicyBack(l) })
	alpine := "a port below 1024 on Lima's Alpine VMs: an Alpine client reaches its link to port 80 on the same port"
	if s.sb.o.wants("from") {
		s.Check(alpine, func() error { return lkAlpine(l) })
	} else {
		s.Skip(alpine, "Alpine's image comes from the from section")
	}
	s.Check("a group is on one tier: an open VM is refused by an allowlist group, and an allowlist one by an open group; two on allowlist reach each other through their group's links",
		func() error { return gpPolicy(l) })
	s.Check("devvm rm of the client leaves no link session, socket, pid file, log or link state", func() error { return lkRmClient(l) })
}

// errorsOf is a failed command's last lines, as an error for a check's
// own to say what it printed: nil when it printed nothing.
func errorsOf(r Res) error {
	if t := tailLines(r.Out, 5); t != "" {
		return errors.New(t)
	}
	return nil
}

func lkMade(l *link) error {
	s := l.s
	for _, v := range []string{l.v1, l.v2, l.vc} {
		if v == "" || !lkRunning(s, v) || s.Paths.State(v)["net.tier"] != "open" {
			if v == "" {
				v = "a VM"
			}
			return errors.New(v + " wasn't made")
		}
	}
	_, forwards := s.Paths.State(l.vc)["ports.forward"]
	switch {
	case s.Paths.State(l.v1)["ports.forward"] != "none" || s.Paths.State(l.v2)["ports.forward"] != "none":
		return errors.New("a target forwards ports")
	case forwards:
		return errors.New("the client doesn't forward every port")
	}
	return nil
}

func lkSshdT(s *S, vms ...string) error {
	for _, vm := range vms {
		r, err := lkRoot(s, vm, `s=$(command -v sshd || echo /usr/sbin/sshd); c=/etc/devvm/ssh/sshd_config; if [ -f $c ]; then $s -T -f $c; else $s -T; fi`)
		if err != nil {
			return errors.New(vm + ": no sshd -T")
		}
		for _, want := range []string{
			"allowtcpforwarding yes", "gatewayports clientspecified", "permitlisten 127.77.*:*",
			"allowstreamlocalforwarding local", "permittunnel no", "x11forwarding no",
		} {
			if !r.Has(want) {
				return errors.New(vm + ": not " + want)
			}
		}
	}
	return nil
}

// lkPublish: $P47243 on its own, as devvm link unpublish takes ports as
// they're listed.
func lkPublish(l *link) error {
	s := l.s
	first := "80," + l.p(0) + "-" + l.p(2) + "," + l.p(4)
	all := first + "," + l.p(3)
	for _, step := range []struct{ dir, ports, said string }{
		{l.p1, first, "==> published ports=" + first + " vm=" + l.v1},
		{l.p1, l.p(3), "==> published ports=" + all + " vm=" + l.v1},
		{l.p2, l.p(0) + ",80", "==> published ports=" + l.p(0) + ",80 vm=" + l.v2},
	} {
		r, err := s.DevvmNames(step.dir, "link", "publish", step.ports)
		if err != nil {
			return err
		}
		if err := said(r, step.said); err != nil {
			return err
		}
	}
	r, _ := s.Devvm(l.p1, "link", "publish")
	switch {
	case out(r) != all:
		return fmt.Errorf("devvm link publish lists %q", out(r))
	case !lkShows(s, l.p1, `^  "version": 1,$`, "link", "publish", "--json"):
		return errors.New(`its --json has no "version": 1`)
	case !lkShows(s, l.p1, `^  "ports": "`+regexp.QuoteMeta(all)+`"$`, "link", "publish", "--json"):
		return errors.New("its --json doesn't list the ports")
	}
	return nil
}

func lkUnpublishedAdd(l *link) error {
	s := l.s
	r, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(9))
	if err == nil {
		return errors.New("devvm link add went on")
	}
	if err := said(r, "devvm: link_not_published port="+l.p(9)+" vm="+l.v1); err != nil {
		return err
	}
	for k := range s.Paths.State(l.vc) {
		if strings.HasPrefix(k, "links.links.") {
			return errors.New("the client has a link")
		}
	}
	return nil
}

func lkAddApplies(l *link) error {
	s := l.s
	start := time.Now()
	r, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(0), "--as", "db")
	if err != nil {
		return err
	}
	if err := said(r, "==> link_added name=db port="+l.p(0)+" target="+l.v1+" vm="+l.vc); err != nil {
		return err
	}
	w := l.w1 + "-" + l.p(0)
	if err := s.Until(15*time.Second, func() error { return lkCurls(s, l.vc, "http://db:"+l.p(0)+"/", w) }); err != nil {
		return err
	}
	Note(s, "reached %d s after devvm link add began", int(time.Since(start).Seconds()))
	if err := lkCurls(s, l.vc, "http://db.devvm.internal:"+l.p(0)+"/", w); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), w); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db.devvm.internal", l.p(0), w); err != nil {
		return err
	}
	if a := lkAddr(s, l.vc, "db"); a != "127.77.0.1" {
		return errors.New("db's address: " + a)
	}
	return nil
}

func lkTwo(l *link) error {
	s := l.s
	r, err := s.DevvmNames(l.pc, "link", "add", l.n2, l.p(0))
	if err != nil {
		return err
	}
	if err := said(r, "==> link_added name="+l.n2+" port="+l.p(0)+" target="+l.v2+" vm="+l.vc); err != nil {
		return err
	}
	if err := s.Until(15*time.Second, func() error { return lkGets(s, l.vc, l.n2, l.p(0), l.w2+"-"+l.p(0)) }); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	if a := lkAddr(s, l.vc, l.n2); a != "127.77.0.2" {
		return errors.New(l.n2 + "'s address: " + a)
	}
	return nil
}

func lkLow(l *link) error {
	s := l.s
	r, err := s.DevvmNames(l.pc, "link", "add", l.n2, "80")
	if err != nil {
		return err
	}
	if err := said(r, "==> link_added name="+l.n2+" port=80 target="+l.v2+" vm="+l.vc); err != nil {
		return err
	}
	u, _ := lkRoot(s, l.vc, "cat /proc/sys/net/ipv4/ip_unprivileged_port_start")
	Note(s, "the client's ip_unprivileged_port_start: %s", out(u))
	if err := s.Until(15*time.Second, func() error { return lkGets(s, l.vc, l.n2, "80", l.w2+"-80") }); err != nil {
		return err
	}
	if !Matches(strings.Join(lkListens(s, l.vc), "\n"), `^sshd[^ ]* 127\.77\.0\.2:80$`) {
		return errors.New("sshd doesn't listen on 127.77.0.2:80")
	}
	return nil
}

func lkList(l *link) error {
	s := l.s
	r, _ := s.Devvm(l.pc, "link")
	Note(s, "%s", r.Stdout)
	q := regexp.QuoteMeta
	if !Matches(r.Stdout, `^db +`+l.p(0)+` +`+q(l.v1)+` +`+q(l.p1)+` +Running +yes$`) ||
		!Matches(r.Stdout, `^`+q(l.n2)+` +80 +`+q(l.v2)+` +`+q(l.p2)+` +Running +yes$`) {
		return errors.New("devvm link doesn't list both links")
	}
	j, _ := s.Devvm(l.pc, "link", "--json")
	if !j.Has(`  "version": 1,`) || !j.Has(`      "name": "db",`) || !j.Has(`      "target": "`+l.v2+`",`) {
		return errors.New("devvm link --json doesn't list them")
	}
	return nil
}

func lkOnRefused(l *link) error {
	s := l.s
	r, err := s.DevvmNames(l.pc, "network", "allowlist")
	if err == nil {
		return errors.New("devvm network allowlist went on")
	}
	q := regexp.QuoteMeta
	for _, v := range []string{l.v1, l.v2} {
		if !Matches(r.Out, `^devvm: tier_linked_targets targets=.*`+q(v)+`.* tier=allowlist vm=`+q(l.vc)+`$`) {
			return errors.New("not said: tier_linked_targets with " + v)
		}
	}
	return not(s.Paths.State(l.vc)["net.tier"] != "open", "the client isn't on open")
}

func lkUnpublish(l *link) error {
	s := l.s
	if _, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(3)); err != nil {
		return err
	}
	if err := s.Until(15*time.Second, func() error { return lkGets(s, l.vc, "db", l.p(3), l.w1+"-"+l.p(3)) }); err != nil {
		return err
	}
	r, err := s.DevvmNames(l.p1, "link", "unpublish", l.p(3))
	if err != nil {
		return err
	}
	if err := said(r, "==> unpublished ports="+l.p(3)+" vm="+l.v1, "unpublished_linked vm="+l.v1+" vms="+l.vc); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(3), ""); err != nil {
		return err
	}
	if err := lkUntilEvent(s, 5, l.vc, "link_refused", "db", l.p(3), `"reason":"unpublished"`); err != nil {
		return err
	}
	q := regexp.QuoteMeta
	if !lkShows(s, l.pc, `^db +`+l.p(3)+` +`+q(l.v1)+` +`+q(l.p1)+` +Running +unpublished$`, "link") {
		return errors.New("devvm link doesn't show the link unpublished")
	}
	return nil
}

func lkRmApplies(l *link) error {
	s := l.s
	start := time.Now()
	r, err := s.DevvmNames(l.pc, "link", "rm", "db", l.p(3))
	if err != nil {
		return err
	}
	if err := said(r, "==> link_removed name=db port="+l.p(3)+" vm="+l.vc); err != nil {
		return err
	}
	addr := "127.77.0.1:" + l.p(3)
	if err := s.Until(15*time.Second, func() error { return not(lkListening(s, l.vc, addr), addr+" still listens") }); err != nil {
		return err
	}
	Note(s, "gone %d s after devvm link rm began", int(time.Since(start).Seconds()))
	if _, err := lkRaw(s, l.vc, "db", l.p(3)); err == nil {
		return errors.New("db:" + l.p(3) + " took the connection")
	}
	return not(Exists(lkSock(s, l.vc, "127.77.0.1", l.p(3))), "its socket is still there")
}

func lkOwnServer(l *link) error {
	s := l.s
	if err := lkStart(l, l.vc, "serve", "127.0.0.1", l.p(0), "own"); err != nil {
		return err
	}
	var h string
	_ = s.Until(20*time.Second, func() error {
		r, _ := s.Run(Cmd{Args: []string{"curl", "-s", "--max-time", "5", "http://127.0.0.1:" + l.p(0) + "/"}})
		h = out(r)
		return not(h != "own", "not the client's own server")
	})
	Note(s, "this machine's 127.0.0.1:%s answered: %s", l.p(0), orNothing(h))
	if h != "own" {
		return errors.New("the client's own server doesn't reach this machine")
	}
	if err := lkCurls(s, l.vc, "http://127.0.0.1:"+l.p(0)+"/", "own"); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	return lkEnd(s, l.vc, "127.0.0.1", l.p(0))
}

func lkSockets(l *link) error {
	s := l.s
	var bad []string
	for _, f := range []string{lkSock(s, l.vc, "127.77.0.1", l.p(0)), lkSock(s, l.vc, "127.77.0.2", l.p(0)), lkSock(s, l.vc, "127.77.0.2", "80")} {
		Note(s, "%s: %s", f, modeOf(f))
		fi, err := os.Stat(f)
		if err != nil || fi.Mode().Type() != os.ModeSocket || modeOf(f) != "600" {
			bad = append(bad, f)
		}
	}
	dir := filepath.Join(s.Paths.Devvm, "run")
	Note(s, "%s: %s", dir, modeOf(dir))
	if modeOf(dir) != "700" {
		bad = append(bad, dir)
	}
	if len(bad) > 0 {
		return errors.New("not a socket of mode 600 in a folder of mode 700: " + strings.Join(bad, " "))
	}
	return lkHostClean(s, l.vc, l.vc, l.v1, l.v2)
}

func lkPortTaken(l *link) error {
	s := l.s
	if err := lkStart(l, l.vc, "serve", "127.77.0.1", l.p(2), "squatter"); err != nil {
		return err
	}
	r, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(2))
	if err != nil {
		return err
	}
	if err := said(r, "devvm: link_port_taken name=db port="+l.p(2)+" vm="+l.vc); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(2), "squatter"); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	if err := lkEnd(s, l.vc, "127.77.0.1", l.p(2)); err != nil {
		return err
	}
	_, err = s.DevvmNames(l.pc, "link", "rm", "db", l.p(2))
	return err
}

// lkFew waits up to a minute for the session's relays to end: fewer than
// 20 processes left.
func lkFew(s *S, vm string) error {
	pid := lkSession(s, vm)
	if pid == "" {
		return errors.New("no link session")
	}
	return s.Until(60*time.Second, func() error {
		return not(len(lkDesc(s, pid)) >= 20, "the link session's relays didn't end")
	})
}

// lkCap: a holding server of its own, started again on a retry: the
// first run's relays end with the old one, before the 256 connections
// are opened. Their ends in the client never take a link check's port
// (lkPrep).
func lkCap(l *link) error {
	s := l.s
	if _, err := lkRoot(s, l.vc, "grep -qx "+l.p(0)+"-"+l.p(9)+" /proc/sys/net/ipv4/ip_local_reserved_ports"); err != nil {
		return errors.New("the client's ports " + l.p(0) + "-" + l.p(9) + " aren't reserved")
	}
	if err := lkEnd(s, l.v1, "127.0.0.1", l.p(1)); err != nil {
		return err
	}
	if err := lkStart(l, l.v1, "hold", "127.0.0.1", l.p(1), ""); err != nil {
		return err
	}
	if err := lkFew(s, l.vc); err != nil {
		return err
	}
	if _, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(1)); err != nil {
		return err
	}
	r, _ := s.InVM(l.vc, "LC_ALL=C perl /var/tmp/e2e-link.pl conns db "+l.p(1)+" 257")
	var got []string
	for _, line := range strings.Split(r.Out, "\n") {
		if strings.HasPrefix(line, "closed:") || strings.HasPrefix(line, "connection ") {
			got = append(got, line)
		}
	}
	lkCapNotes(l)
	err := errors.Join(
		not(strings.Join(got, "\n") != "closed: 257", "not only the 257th closed: "+strings.Join(got, " | ")),
		not(!lkEvent(s, l.vc, "link_limit", "db", l.p(1), `"reason":"256"`), "no link_limit event"),
		not(!lkShows(s, l.pc, `  REFUSED  db:`+l.p(1)+`  256 connections carried already$`, "network", "log", "-n", "100000"), "devvm network log doesn't show the refusal"))
	// Their relays end once the target's server lets them go. The link
	// goes whatever came of the checks: the group's links, later, would
	// leave it out.
	err = errors.Join(err, lkEnd(s, l.v1, "127.0.0.1", l.p(1)), lkFew(s, l.vc))
	_, rmErr := s.DevvmNames(l.pc, "link", "rm", "db", l.p(1))
	return errors.Join(err, rmErr)
}

// lkRmEnds: a connection that a program in the client holds open, to a
// target's server that never closes one (hold), is carried by an ssh of
// the link session's on this machine until devvm link rm, which ends it.
// Its port is its own, published for it alone and unpublished after, so
// the group's checks later see db's ports as they were.
func lkRmEnds(l *link) error {
	s := l.s
	p := l.p(7)
	if _, err := s.DevvmNames(l.p1, "link", "publish", p); err != nil {
		return err
	}
	defer func() {
		_, _ = s.Devvm(l.pc, "link", "rm", "db", p) // gone already once the check passed
		_, _ = s.Devvm(l.p1, "link", "unpublish", p)
	}()
	if err := lkStart(l, l.v1, "hold", "127.0.0.1", p, ""); err != nil {
		return err
	}
	defer func() {
		_, _ = lkRoot(s, l.vc, "pkill -f '[e]2e-link-held' || true")
		_ = lkEnd(s, l.v1, "127.0.0.1", p)
	}()
	if _, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", p); err != nil {
		return err
	}
	addr := "127.77.0.1:" + p
	if err := s.Until(15*time.Second, func() error { return not(!lkListening(s, l.vc, addr), addr+" doesn't listen") }); err != nil {
		return err
	}
	if err := lkBg(s, l.vc, "bash -c 'exec 3<>/dev/tcp/db/"+p+" && exec -a e2e-link-held sleep 300'"); err != nil {
		return err
	}
	// The session's ssh -W relays to the port.
	relays := func() []string {
		mine := lkDesc(s, lkSession(s, l.vc))
		var r []string
		for _, pid := range pids(s, "[-]W 127.0.0.1:"+p+" ") {
			if slices.Contains(mine, pid) {
				r = append(r, pid)
			}
		}
		return r
	}
	if err := s.Until(20*time.Second, func() error { return not(len(relays()) != 1, "no relay for the held connection") }); err != nil {
		return err
	}
	if _, err := s.DevvmNames(l.pc, "link", "rm", "db", p); err != nil {
		return err
	}
	start := time.Now()
	err := s.Until(15*time.Second, func() error { return not(len(relays()) > 0, "its relay still runs: "+strings.Join(relays(), " ")) })
	Note(s, "the relay ended %d s after devvm link rm", int(time.Since(start).Seconds()))
	return err
}

// lkCapNotes logs the events for db:$P47241, their reasons and the end of
// the link session's log.
func lkCapNotes(l *link) {
	s := l.s
	of := lkOf(lkEvents(s, l.vc, false), "db", l.p(1))
	ev, reason := regexp.MustCompile(`"event":"([a-z_]*)"`), regexp.MustCompile(`"event":"([a-z_]*)".*"reason":"([^"]*)"`)
	counts, reasons := map[string]int{}, map[string]int{}
	for _, e := range of {
		if m := ev.FindStringSubmatch(e); m != nil {
			counts[m[1]]++
		}
		if m := reason.FindStringSubmatch(e); m != nil && !strings.Contains(e, `"event":"link",`) {
			reasons[m[1]+": "+m[2]]++
		}
	}
	Note(s, "events for db:%s: %v", l.p(1), counts)
	Note(s, "their reasons: %v", reasons)
	b, _ := os.ReadFile(filepath.Join(s.Paths.VMDir(l.vc), "logs", "links.log"))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	Note(s, "the session's log: %s", strings.Join(lines[max(0, len(lines)-3):], "|"))
}

// lkHostileListen: without the link to port 80, which GatewayPorts yes
// would put on the client's every address, and its forwarder on this
// machine's port 80.
func lkHostileListen(l *link) error {
	s := l.s
	if _, err := s.DevvmNames(l.pc, "link", "rm", l.n2, "80"); err != nil {
		return err
	}
	if err := lkHostile(s, l.vc, ""); err != nil {
		return err
	}
	// The control: sshd now binds an address it refused.
	if _, err := s.Run(Cmd{Args: append(lkSSH(s), "-o", "ExitOnForwardFailure=yes", "-R", "127.0.0.1:"+l.p(8)+":127.0.0.1:9", l.vc, "true")}); err != nil {
		return errors.New("sshd still refuses 127.0.0.1")
	}
	if err := lkReconnect(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	if err := s.Until(15*time.Second, func() error { return lkGets(s, l.vc, l.n2, l.p(0), l.w2+"-"+l.p(0)) }); err != nil {
		return err
	}
	if err := lkVMClean(s, l.vc, l.p(0)); err != nil {
		return err
	}
	return lkHostClean(s, l.vc, l.vc, l.v1, l.v2)
}

// lkHostileGateway: with GatewayPorts yes, sshd binds every remote
// forward on the client's every address, whatever the host asked: db's on
// 0.0.0.0:$P47240, so the second target's on the same port can't bind.
// What it shows is recorded; root in the client can put a relay of its
// own there as well.
func lkHostileGateway(l *link) error {
	s := l.s
	l.recorded = ""
	if err := lkHostile(s, l.vc, "gateway"); err != nil {
		return err
	}
	if err := lkReconnect(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	var on []string
	for _, line := range lkListens(s, l.vc) {
		if strings.HasSuffix(line, ":"+l.p(0)) {
			on = append(on, line)
		}
	}
	l.recorded = "the client's listeners on " + l.p(0) + ": " + strings.Join(on, " ")
	Note(s, "%s", l.recorded)
	return lkHostClean(s, l.vc, l.vc, l.v1, l.v2)
}

func lkLogOff(l *link) error {
	s := l.s
	r, _ := s.Devvm(l.pc, "network", "log", "-n", "100000")
	q := regexp.QuoteMeta
	switch {
	case !Matches(r.Stdout, `  link +db:`+l.p(0)+`  to `+q(l.v1)+`$`):
		return errors.New("devvm network log doesn't show db's connections")
	case !Matches(r.Stdout, `  link +`+q(l.n2)+`:80  to `+q(l.v2)+`$`):
		return errors.New("devvm network log doesn't show " + l.n2 + ":80's connections")
	case !Matches(r.Stdout, `  REFUSED  db:`+l.p(3)+`  to `+q(l.v1)+`: unpublished$`):
		return errors.New("devvm network log doesn't show the unpublished port's refusal")
	case !lkEvent(s, l.vc, "link", "db", l.p(0), `"target":"`+l.v1+`"`):
		return errors.New("no link event for db")
	}
	return nil
}

// lkStopped: the link session reuses what the target's backend said of
// its run for a second, so a connection within a second of the stop may
// not see it: it connects again until one does.
func lkStopped(l *link) error {
	s := l.s
	if _, err := s.DevvmNames(l.p2, "stop"); err != nil {
		return err
	}
	if err := s.Until(30*time.Second, func() error { return not(lkRunning(s, l.v2), l.v2+" runs") }); err != nil {
		return err
	}
	if err := s.Until(5*time.Second, func() error {
		if err := lkGets(s, l.vc, l.n2, l.p(0), ""); err != nil {
			return err
		}
		return not(!lkEvent(s, l.vc, "link_target_stopped", l.n2, l.p(0), ""), "no link_target_stopped event")
	}); err != nil {
		return err
	}
	r, err := lkUp(s, l.pc)
	if err != nil {
		return err
	}
	if err := said(r, "devvm: link_target_stopped target="+l.v2+" vm="+l.vc); err != nil {
		return err
	}
	// The client's launch mustn't start its target: once its link session
	// has refused the stopped target, the target still doesn't run.
	q := regexp.QuoteMeta
	if err := s.Until(30*time.Second, func() error {
		return not(!lkShows(s, l.pc, `  REFUSED  `+q(l.n2)+`:`+l.p(0)+`  to `+q(l.v2)+`: it isn't running$`, "network", "log", "-n", "100000"),
			"devvm network log doesn't show the refusal")
	}); err != nil {
		return err
	}
	if !lkShows(s, l.pc, `^`+q(l.n2)+` +`+l.p(0)+` +`+q(l.v2)+` +`+q(l.p2)+` +Stopped +yes$`, "link") {
		return errors.New("devvm link doesn't show the target stopped")
	}
	return not(lkRunning(s, l.v2), l.v2+" runs")
}

func lkRestarted(l *link) error {
	s := l.s
	k0 := lkKnownHosts(s, l.v2)
	r, err := lkUp(s, l.p2)
	if err == nil && !lkRunning(s, l.v2) {
		err = errors.New(l.v2 + " doesn't run")
	}
	if err == nil {
		err = lkServe(l, l.v2, l.w2, l.p(0), "80")
	}
	if err != nil {
		return errors.Join(err, errorsOf(r))
	}
	k1 := lkKnownHosts(s, l.v2)
	if err := s.Until(20*time.Second, func() error { return lkGets(s, l.vc, l.n2, l.p(0), l.w2+"-"+l.p(0)) }); err != nil {
		return err
	}
	// Lima keeps a VM's host keys.
	Note(s, "host key kept: %s", yesNo(k0 == k1))
	return nil
}

func lkRmTarget(l *link) error {
	s := l.s
	sess := lkSession(s, l.vc)
	r, err := s.DevvmNames(l.p2, "rm", "--force", "--no-export")
	if err != nil {
		return err
	}
	if err := said(r, "==> links_target_gone count=1 vm="+l.v2+" vms="+l.vc); err != nil {
		return err
	}
	for k, v := range s.Paths.State(l.vc) {
		if Matches(k, `^links\.links\.[0-9]*\.target$`) && strings.Contains(v, l.v2) {
			return errors.New("the client still links to " + l.v2)
		}
	}
	if err := s.Until(15*time.Second, func() error { return not(lkResolves(s, l.vc, l.n2), l.n2+" resolves") }); err != nil {
		return err
	}
	sock := lkSock(s, l.vc, "127.77.0.2", l.p(0))
	if err := s.Until(15*time.Second, func() error { return not(Exists(sock), sock+" is still there") }); err != nil {
		return err
	}
	switch {
	case Exists(lkSock(s, l.vc, "127.77.0.2", "80")):
		return errors.New("port 80's socket is still there")
	case sess == "" || lkSession(s, l.vc) != sess:
		return errors.New("the client's session didn't run on")
	}
	return lkGets(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0))
}

// lkLinkState is the VM's link state, as sorted key=value lines.
func lkLinkState(s *S, vm string) string {
	var lines []string
	for k, v := range s.Paths.State(vm) {
		if strings.HasPrefix(k, "links.links.") {
			lines = append(lines, k+"="+v)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func lkMadeAgain(l *link) error {
	s := l.s
	id0 := lkLinkState(s, l.vc)
	vm, _, _ := s.Launch(l.p2, "exit", "--network", "open", "--no-git-protect", "--cpus", "2", "--memory", "2", "--ports", "none")
	if vm != l.v2 || !lkRunning(s, l.v2) {
		return errors.New("not made again: " + orNothing(vm))
	}
	switch {
	case lkShows(s, l.pc, regexp.QuoteMeta(l.v2), "link"):
		return errors.New("devvm link lists " + l.v2)
	case lkResolves(s, l.vc, l.n2):
		return errors.New(l.n2 + " resolves in the client")
	case lkLinkState(s, l.vc) != id0:
		return errors.New("the client's links changed")
	}
	if _, err := s.DevvmNames(l.p2, "link", "publish", l.p(0)); err != nil {
		return err
	}
	return lkServe(l, l.v2, l.w2, l.p(0))
}

// lkPolicyOn: the target gets network policy first, while it runs: until
// its next start it runs outside its proxy, its servers still up, and the
// client's link to it is refused then, and warned of by devvm link add
// and the client's launch (lkPolicyUnenforced).
func lkPolicyOn(l *link) error {
	s := l.s
	r, err := s.DevvmNames(l.p1, "network", "allowlist")
	if err != nil || s.Paths.State(l.v1)["net.tier"] != "allowlist" {
		return errors.Join(errors.New(l.v1+": not on allowlist"), err, errorsOf(r))
	}
	r, err = s.DevvmNames(l.pc, "network", "allowlist")
	if err == nil {
		r, err = lkUp(s, l.pc, "restart")
	}
	st := s.Paths.State(l.vc)
	if err != nil || st["net.tier"] != "allowlist" || st["net.made.check"] != "ok" {
		return errors.Join(errors.New(l.vc+": not restarted behind its proxy"), err, errorsOf(r))
	}
	return nil
}

func lkPolicyUnenforced(l *link) error {
	s := l.s
	if !lkShows(s, l.p1, `^`+regexp.QuoteMeta(l.v1)+` +Running .* unenforced `, "ls") {
		return errors.New("devvm ls doesn't show it unenforced")
	}
	if _, err := lkRoot(s, l.v1, "test -s /var/tmp/e2e-link-127.0.0.1-"+l.p(0)+".pid"); err != nil {
		return errors.New("the target's server isn't up")
	}
	if err := lkGets(s, l.vc, "db", l.p(0), ""); err != nil {
		return err
	}
	if err := lkUntilEvent(s, 5, l.vc, "link_refused", "db", l.p(0), `"reason":"unenforced"`); err != nil {
		return err
	}
	// devvm link add and the client's launch warn of it, refusing nothing.
	warn := "devvm: link_target_unenforced target=" + l.v1 + " vm=" + l.vc
	r, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(0))
	if err != nil {
		return err
	}
	if err := said(r, "==> link_exists name=db port="+l.p(0)+" vm="+l.vc, warn); err != nil {
		return err
	}
	if r, err = lkUp(s, l.pc); err != nil {
		return err
	}
	return said(r, warn)
}

// lkPolicyReach: the client got its link before its restart with network
// policy; lkProxyAdded adds one while it runs.
func lkPolicyReach(l *link) error {
	s := l.s
	if _, err := s.InVM(l.vc, `[ -n "$HTTP_PROXY" ]`); err != nil {
		return errors.New("no proxy settings")
	}
	w := l.w1 + "-" + l.p(0)
	if err := s.Until(20*time.Second, func() error { return lkCurls(s, l.vc, "http://db:"+l.p(0)+"/", w) }); err != nil {
		return err
	}
	if err := lkCurls(s, l.vc, "http://db.devvm.internal:"+l.p(0)+"/", w); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), w); err != nil {
		return err
	}
	_, err := s.InVM(l.vc, "curl --noproxy '*' -sS -m 5 -o /dev/null http://1.1.1.1/")
	return not(err == nil, "a direct connection went out")
}

func lkProxyAdded(l *link) error {
	s := l.s
	b0, k0 := lkBoot(s, l.vc), lkKnownHosts(s, l.vc)
	n, _ := s.InVM(l.vc, `printf "%s|%s" "$NO_PROXY" "$no_proxy"`)
	Note(s, "NO_PROXY|no_proxy: %s", n.Stdout)
	if n.Stdout != "localhost,127.0.0.1,::1|localhost,127.0.0.1,::1" {
		return errors.New("NO_PROXY isn't localhost,127.0.0.1,::1")
	}
	w := l.w1 + "-" + l.p(2)
	if err := lkStart(l, l.v1, "serve", "127.0.0.1", l.p(2), w); err != nil {
		return err
	}
	r, err := s.DevvmNames(l.pc, "link", "add", l.x+"db", l.p(2), "--as", "db")
	if err != nil {
		return err
	}
	if err := said(r, "==> link_added name=db port="+l.p(2)+" target="+l.v1+" vm="+l.vc); err != nil {
		return err
	}
	u := "http://db:" + l.p(2) + "/"
	if err := s.Until(15*time.Second, func() error { return lkViaGets(s, l.vc, u, w, false) }); err != nil {
		return err
	}
	if err := errors.Join(lkViaGets(s, l.vc, "http://db.devvm.internal:"+l.p(2)+"/", w, false), lkViaGets(s, l.vc, u, w, true),
		lkCurls(s, l.vc, u, w), lkGets(s, l.vc, "db", l.p(2), w)); err != nil {
		return err
	}
	b1, k1 := lkBoot(s, l.vc), lkKnownHosts(s, l.vc)
	Note(s, "boot and init before: %s, after: %s", b0, b1)
	np, _ := s.InVM(l.vc, `printf "%s" "$NO_PROXY"`)
	switch {
	case b0 == "" || b0 != b1 || k0 != k1:
		return errors.New("the client restarted")
	case np.Stdout != "localhost,127.0.0.1,::1":
		return errors.New("NO_PROXY changed: " + np.Stdout)
	}
	return not(!lkEvent(s, l.vc, "link", "db", l.p(2), `"target":"`+l.v1+`"`), "no link event")
}

func lkProxyOwn(l *link) error {
	s := l.s
	count := func() int {
		n := 0
		for _, e := range lkEvents(s, l.vc, false) {
			if strings.Contains(e, `"event":"link`) {
				n++
			}
		}
		return n
	}
	l0 := count()
	for _, u := range []string{
		"http://" + l.x + "db:" + l.p(0) + "/", "http://db:" + l.p(4) + "/", "http://db:" + l.p(3) + "/",
		"http://db.devvm.internal.devvm.internal:" + l.p(0) + "/", "http://devvm.internal:" + l.p(0) + "/", "http://127.77.0.1:" + l.p(0) + "/",
	} {
		for _, connect := range []bool{false, true} {
			c, _ := lkVia(s, l.vc, u, connect)
			Note(s, "%s (CONNECT: %v): %s", u, connect, c)
			if c != "403" {
				return fmt.Errorf("%s (CONNECT: %v): %s, not 403", u, connect, c)
			}
		}
	}
	l1 := count()
	Note(s, "link events: %d before, %d after", l0, l1)
	// The target has network policy and no links: the client's link names
	// are nothing to its proxy.
	for _, connect := range []bool{false, true} {
		if c, _ := lkVia(s, l.v1, "http://db:"+l.p(0)+"/", connect); c != "403" {
			return fmt.Errorf("the target's proxy, http://db:%s/ (CONNECT: %v): %s", l.p(0), connect, c)
		}
	}
	if l0 != l1 {
		return errors.New("the refusals were logged as links")
	}
	return lkViaGets(s, l.vc, "http://db:"+l.p(0)+"/", l.w1+"-"+l.p(0), false)
}

func lkLogOn(l *link) error {
	s := l.s
	t0 := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if err := lkGets(s, l.vc, "db", l.p(0), l.w1+"-"+l.p(0)); err != nil {
		return err
	}
	if s.Paths.State(l.vc)["net.tier"] != "allowlist" {
		return errors.New("the client isn't on allowlist")
	}
	var last string
	for _, e := range lkEvents(s, l.vc, true) {
		if strings.Contains(e, `"event":"link","host":"db","port":`+l.p(0)+`,`) {
			last = e
		}
	}
	Note(s, "the last: %s, since %s", orNothing(last), t0)
	m := regexp.MustCompile(`^\{"time":"([^"]*)"`).FindStringSubmatch(last)
	if m == nil || m[1] < t0 {
		return errors.New("no link event since " + t0)
	}
	if !lkShows(s, l.pc, `  link +db:`+l.p(0)+`  to `+regexp.QuoteMeta(l.v1)+`$`, "network", "log", "-n", "100000") {
		return errors.New("devvm network log doesn't show db's connections")
	}
	return nil
}

// lkUpOK launches devvm in dir, failing with what it printed.
func lkUpOK(s *S, what, dir string) (Res, error) {
	r, err := lkUp(s, dir)
	if err != nil {
		return r, errors.Join(errors.New("with the target on "+what), err, errorsOf(r))
	}
	return r, nil
}

// lkTiers: the tier rule both ways: the client on allowlist links to a
// stricter target, none, whose connections go on, and not to a lower
// one, internet, whose next connection is refused; the target back on
// allowlist, they go on again.
func lkTiers(l *link) error {
	s := l.s
	u, w := "http://db:"+l.p(0)+"/", l.w1+"-"+l.p(0)
	curls := func() error { return lkCurls(s, l.vc, u, w) }
	if err := lkSetNet(s, l.v1, "none"); err != nil {
		return err
	}
	if _, err := lkUpOK(s, "none", l.p1); err != nil {
		return err
	}
	if err := s.Until(20*time.Second, curls); err != nil {
		return errors.Join(errors.New("with the target on none"), err)
	}
	if err := lkSetNet(s, l.v1, "internet"); err != nil {
		return err
	}
	if _, err := lkUpOK(s, "internet", l.p1); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), ""); err != nil {
		return err
	}
	if err := lkUntilEvent(s, 5, l.vc, "link_refused", "db", l.p(0), `"reason":"policy"`); err != nil {
		return err
	}
	r, err := lkUp(s, l.pc)
	if err != nil {
		return err
	}
	if err := said(r, "devvm: link_tier_mismatch target="+l.v1+" target_tier=internet tier=allowlist vm="+l.vc); err != nil {
		return err
	}
	if err := lkSetNet(s, l.v1, "allowlist"); err != nil {
		return err
	}
	if _, err := lkUp(s, l.p1); err != nil {
		return err
	}
	if _, err := lkUp(s, l.pc); err != nil {
		return err
	}
	return s.Until(20*time.Second, curls)
}

func lkPolicyBroken(l *link) error {
	s := l.s
	if err := lkSetNet(s, l.v1, "off"); err != nil {
		return err
	}
	if err := lkGets(s, l.vc, "db", l.p(0), ""); err != nil {
		return err
	}
	if err := lkUntilEvent(s, 5, l.vc, "link_refused", "db", l.p(0), `"reason":"policy"`); err != nil {
		return err
	}
	r, err := lkUp(s, l.pc)
	if err != nil {
		return err
	}
	if err := said(r, "devvm: link_tier_mismatch target="+l.v1+" target_tier=open tier=allowlist vm="+l.vc); err != nil {
		return err
	}
	if !lkRunning(s, l.vc) {
		return errors.New("the client doesn't run")
	}
	return s.Until(10*time.Second, func() error { return not(lkResolves(s, l.vc, "db"), "db resolves") })
}

func lkPolicyBack(l *link) error {
	s := l.s
	if err := lkSetNet(s, l.v1, "on"); err != nil {
		return err
	}
	r, err := lkUp(s, l.pc)
	if err != nil {
		return err
	}
	if strings.Contains(r.Out, "link_tier_mismatch") {
		return errors.New("the launch warned of link_tier_mismatch")
	}
	if err := s.Until(10*time.Second, func() error { return not(!lkResolves(s, l.vc, "db"), "db doesn't resolve") }); err != nil {
		return err
	}
	return s.Until(20*time.Second, func() error { return lkCurls(s, l.vc, "http://db:"+l.p(0)+"/", l.w1+"-"+l.p(0)) })
}

func lkAlpine(l *link) error {
	s := l.s
	pa, err := s.NewProject(l.x + "alp")
	if err != nil {
		return err
	}
	defer func() { _, _ = s.Devvm(pa, "rm", "--force", "--no-export") }()
	env, err := NoDocker(s)
	if err != nil {
		return err
	}
	va, r, _ := launchWith(s, pa, env, "--from", fromAlpine, "--network", "open", "--no-git-protect", "--cpus", "2", "--memory", "2", "--ports", "none")
	if va == "" || s.Paths.State(va)["tools.made.libc"] != "musl" {
		return errors.Join(errors.New("no Alpine VM"), errorsOf(r))
	}
	r, err = s.DevvmNames(pa, "link", "add", l.x+"db", "80")
	if err != nil {
		return err
	}
	if err := said(r, "==> link_added name="+l.x+"db port=80 target="+l.v1+" vm="+va); err != nil {
		return err
	}
	u, _ := lkRoot(s, va, "cat /proc/sys/net/ipv4/ip_unprivileged_port_start")
	Note(s, "its ip_unprivileged_port_start: %s", out(u))
	return s.Until(20*time.Second, func() error { return lkGets(s, va, l.x+"db", "80", l.w1+"-80") })
}

func lkRmClient(l *link) error {
	s := l.s
	if _, err := s.DevvmNames(l.pc, "rm", "--force", "--no-export"); err != nil {
		return err
	}
	if err := s.Until(10*time.Second, func() error { return not(lkSession(s, l.vc) != "", "its session runs") }); err != nil {
		return err
	}
	if m, _ := filepath.Glob(filepath.Join(s.Paths.Devvm, "run", "link-*")); len(m) > 0 {
		return errors.New("left: " + strings.Join(m, " "))
	}
	if Exists(s.Paths.VMDir(l.vc)) {
		return errors.New("its folder is still there")
	}
	m, _ := filepath.Glob(filepath.Join(s.Paths.Devvm, "vms", "*", "vm.json"))
	for _, f := range m {
		for k, v := range readState(f) {
			if Matches(k, `^links\.links\.[0-9]*\.target$`) && v == l.vc {
				return errors.New(f + " still links to it")
			}
		}
	}
	return nil
}

// linkTools are the host commands the link section uses: nc isn't, as
// the runner dials itself, and lsof or ss lists the link session's TCP.
func linkTools() []tool {
	t := []tool{{"curl", "curl", "curl", "curl"}, sshTool(), {"pgrep", "proctools", "procps", "procps-ng"}}
	if runtime.GOOS == "darwin" {
		return append(t, tool{"lsof", "lsof", "-", "-"})
	}
	return append(t, tool{"ss", "-", "iproute2", "iproute"})
}
