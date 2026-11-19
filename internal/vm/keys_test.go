package vm

import (
	"cmp"
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// The names tests read and change a VM's state by (get, set), each one
// field of vm.json: a core field, or one of a section's.
// The names vm itself had for them are kept for tests (KeyNet…).
const (
	KeyNet                = keyNet
	KeyTools              = keyTools
	KeyLibc               = keyLibc
	KeyPorts              = keyPorts
	KeyMode               = keyClone
	KeySeeded             = keySeeded
	KeyCloneRemote        = keyCloneRemote
	KeyPublish            = keyPublish
	KeyGroup              = keyGroup
	KeyGroupAs            = keyGroupAs
	KeyLinks              = keyLinks
	KeyAgentToggle        = keyAgentToggle
	KeyDevcontainer       = keyDevcontainer
	KeyDevcontainerEnv    = keyDevEnv
	KeyDevcontainerCreate = keyDevCreate
	KeyDesktopPassword    = keyDesktopPass
	KeyDesktopPort        = keyDesktopPort
	ModeClone             = "clone"
)

const (
	keyNet          = "net"          // netpolicy's Tier: open, internet, allowlist or none
	keyCheck        = "net_check"    // netpolicy's Made.Check
	keyProxyKey     = "proxy_key"    // netpolicy's Made.ProxyKey
	keySSHPort      = "ssh_port"     // ports' Host.SSH
	keyProxyPort    = "proxy_port"   // ports' Host.Proxy
	keyDesktopPort  = "desktop_port" // ports' Host.Desktop
	keyPorts        = "ports"        // ports' Forward
	keyImage        = "image"        // the core field
	keyDir          = "dir"          // the core field
	keyFolder       = "folder"       // the core field
	keyFrom         = "from"         // the core field
	keyBackend      = "backend"      // the core field
	keyID           = "id"           // the core field
	keyToolNames    = "tool_names"   // tools' Names, space-separated
	keyTools        = "tools"        // tools' Made.Files, space-separated
	keyLibc         = "libc"         // tools' Made.Libc
	keyGitProtect   = "git_protect"  // gitprotect's Mode
	keyPublish      = "publish"      // linksInVM.Publish
	keyGroup        = "group"        // linksInVM.Group
	keyGroupAs      = "group_as"     // linksInVM.GroupAs
	keyLinks        = "links"        // linksInVM.Links as NAME,PORT,TARGET,ID,ADDR[,group] entries
	keyCPUs         = "cpus"         // Size.CPUs
	keyMemory       = "memory"       // Size.Memory
	keyDisk         = "disk"         // Size.Disk
	keyAgent        = "ssh_agent"    // sshagent's Mode
	keyAgentToggle  = "agent_toggle" // sshagent's Made.Toggle: on, or ""
	keyClone        = "mode"         // cloneInVM.On: clone, or ""
	keySeeded       = "seeded"       // cloneInVM.Made.Seeded: yes, or ""
	keyCloneRemote  = "clone_remote" // cloneInVM.Made.Remote
	keySecrets      = "secrets"      // secrets' Names, space-separated
	keyDevcontainer = "devcontainer" // "<sum> <file>", none or off
	keyDevEnv       = "devcontainer_env"
	keyDevCreate    = "devcontainer_create" // devcontainerInVM.Create as JSON, until it ran
	keyDesktopPass  = "desktop_password"
)

// get returns one field of a VM's state, as a string ("" for none).
func get(st *state.VM, key string) string {
	n := sectionOf[netpolicy.Net](st, netpolicy.Section)
	p := sectionOf[ports.Ports](st, ports.Section)
	tl := sectionOf[tools.InVM](st, tools.Section)
	l := linksOf(st)
	s := sectionOf[Size](st, sizeSection)
	c := sectionOf[cloneInVM](st, cloneSection)
	d := sectionOf[devcontainerInVM](st, devcontainerSection)
	a := sectionOf[sshagent.InVM](st, sshagent.Section)
	num := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	yes := func(b bool, v string) string {
		if b {
			return v
		}
		return ""
	}
	switch key {
	case keyNet:
		if _, ok := sectionRaw(st, netpolicy.Section); !ok {
			return ""
		}
		return string(n.Tier)
	case keyCheck:
		return n.Made.Check
	case keyProxyKey:
		return n.Made.ProxyKey.Reveal()
	case keySSHPort:
		return num(p.Host.SSH)
	case keyProxyPort:
		return num(p.Host.Proxy)
	case keyDesktopPort:
		return num(p.Host.Desktop)
	case keyPorts:
		return p.Forward
	case keyImage:
		return st.Image
	case keyDir:
		return st.Dir
	case keyFolder:
		return st.Folder()
	case keyFrom:
		return st.From
	case keyBackend:
		return st.Backend
	case keyID:
		return st.ID
	case keyToolNames:
		return strings.Join(tl.Names, " ")
	case keyTools:
		return strings.Join(tl.Made.Files, " ")
	case keyLibc:
		return tl.Made.Libc
	case keyGitProtect:
		return gitprotect.Mode(st)
	case keyPublish:
		return l.Publish
	case keyGroup:
		return l.Group
	case keyGroupAs:
		return l.GroupAs
	case keyLinks:
		var es []string
		for _, k := range l.Links {
			es = append(es, formatLink(k))
		}
		return strings.Join(es, " ")
	case keyCPUs:
		return s.CPUs
	case keyMemory:
		return s.Memory
	case keyDisk:
		return s.Disk
	case keyAgent:
		return a.Mode
	case keyAgentToggle:
		return yes(a.Made.Toggle, "on")
	case keyClone:
		return yes(c.On, "clone")
	case keySeeded:
		return yes(c.Made.Seeded, "yes")
	case keyCloneRemote:
		return c.Made.Remote
	case keySecrets:
		b, _ := secrets.Of(st)
		return strings.Join(b.Names, " ")
	case keyDevcontainer:
		switch {
		case d.Off:
			return "off"
		case d.File == "":
			return "none"
		}
		return d.Sum + " " + d.File
	case keyDevEnv:
		return jsonText(d.Env)
	case keyDevCreate:
		if d.Made.Created {
			return ""
		}
		return jsonText(d.Create)
	case keyDesktopPass:
		return sectionOf[desktopInVM](st, desktopSection).Made.Password
	}
	panic("no state field " + key)
}

// formatLink is a link as tests show it: NAME,PORT,TARGET,ID,ADDR, then
// ,group when its group made it.
func formatLink(l Link) string {
	e := l.Name + "," + strconv.Itoa(l.Port) + "," + l.Target.String() + "," + l.TargetID + "," + l.Addr.String()
	if l.Group {
		e += ",group"
	}
	return e
}

// sectionRaw reports whether st has a section, by its decoded JSON.
func sectionRaw(st *state.VM, name string) (json.RawMessage, bool) {
	raw, err := state.Section[json.RawMessage](st, name)
	return raw, err == nil && raw != nil
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	if s := string(b); s != "null" && s != "[]" {
		return s
	}
	return ""
}

// set changes one field of a VM's state (get's), as devvm does, under its
// lock; a test fails when it can't.
func set(t *testing.T, st *state.VM, key, val string) error {
	t.Helper()
	change(t, st, func(st *state.VM) error { return setField(st, key, val) })
	return nil
}

// setField sets one field of a VM's state, in st alone.
func setField(st *state.VM, key, val string) error {
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	np := func(f func(*netpolicy.Net)) error {
		n, err := netpolicy.Of(st)
		if err != nil {
			return err
		}
		f(&n)
		return netpolicy.Set(st, n)
	}
	pp := func(f func(*ports.Ports)) error {
		p, err := ports.Of(st)
		if err != nil {
			return err
		}
		f(&p)
		return ports.Set(st, p)
	}
	tp := func(f func(*tools.InVM)) error {
		in, err := tools.Of(st)
		if err != nil {
			return err
		}
		f(&in)
		return tools.Set(st, in)
	}
	ap := func(f func(*sshagent.InVM)) error {
		a, err := sshagent.Of(st)
		if err != nil {
			return err
		}
		f(&a)
		return sshagent.Set(st, a)
	}
	lp := func(f func(*linksInVM)) error {
		return edit(st, linksSection, func(l *linksInVM) error { f(l); return nil })
	}
	switch key {
	case keyNet:
		if val == "" {
			return state.SetSection(st, netpolicy.Section, struct{}{})
		}
		return np(func(n *netpolicy.Net) { n.Tier = netpolicy.Tier(val) })
	case keyCheck:
		return np(func(n *netpolicy.Net) { n.Made.Check = val })
	case keySSHPort:
		return pp(func(p *ports.Ports) { p.Host.SSH = atoi(val) })
	case keyProxyPort:
		return pp(func(p *ports.Ports) { p.Host.Proxy = atoi(val) })
	case keyDesktopPort:
		return pp(func(p *ports.Ports) { p.Host.Desktop = atoi(val) })
	case keyPorts:
		return pp(func(p *ports.Ports) { p.Forward = val })
	case keyImage:
		st.Image = val
	case keyDir:
		st.Dir = val
	case keyFolder:
		st.SetFolder(val, st.FolderVolume())
	case keyFrom:
		st.From = val
	case keyID:
		st.ID = val
	case keyToolNames:
		return tp(func(in *tools.InVM) { in.Names = strings.Fields(val) })
	case keyTools:
		return tp(func(in *tools.InVM) { in.Made.Files = strings.Fields(val) })
	case keyLibc:
		return tp(func(in *tools.InVM) { in.Made.Libc = val })
	case keyGitProtect:
		g, err := gitprotect.Of(st)
		if err != nil {
			return err
		}
		g.Mode = val
		return gitprotect.Set(st, g)
	case keyPublish:
		return lp(func(l *linksInVM) { l.Publish = val })
	case keyGroup:
		return lp(func(l *linksInVM) { l.Group = val })
	case keyGroupAs:
		return lp(func(l *linksInVM) { l.GroupAs = val })
	case keyLinks:
		var links []Link
		for e := range strings.FieldsSeq(val) {
			f := strings.Split(e, ",")
			for len(f) < 5 {
				f = append(f, "")
			}
			addr, _ := netip.ParseAddr(f[4])
			links = append(links, Link{Name: f[0], Port: atoi(f[1]), Target: vmname.VM(f[2]), TargetID: f[3], Addr: addr, Group: len(f) > 5 && f[5] == "group"})
		}
		return lp(func(l *linksInVM) { l.Links = links })
	case keyCPUs, keyMemory, keyDisk:
		return edit(st, sizeSection, func(s *Size) error {
			*map[string]*string{keyCPUs: &s.CPUs, keyMemory: &s.Memory, keyDisk: &s.Disk}[key] = val
			return nil
		})
	case keyAgent:
		return ap(func(a *sshagent.InVM) { a.Mode = val })
	case keyAgentToggle:
		return ap(func(a *sshagent.InVM) { a.Made.Toggle = val != "" })
	case keyClone:
		return edit(st, cloneSection, func(c *cloneInVM) error { c.On = val == "clone"; return nil })
	case keySeeded:
		return edit(st, cloneSection, func(c *cloneInVM) error { c.Made.Seeded = val != ""; return nil })
	case keyCloneRemote:
		return edit(st, cloneSection, func(c *cloneInVM) error { c.Made.Remote = val; return nil })
	case keySecrets:
		return state.SetSection(st, secrets.Section, secrets.Bound{Names: strings.Fields(val)})
	case keyDevEnv:
		return edit(st, devcontainerSection, func(d *devcontainerInVM) error {
			d.Env = nil
			return json.Unmarshal([]byte(cmp.Or(val, "null")), &d.Env)
		})
	case keyDevCreate:
		return edit(st, devcontainerSection, func(d *devcontainerInVM) error {
			d.Create, d.Made.Created = nil, false
			return json.Unmarshal([]byte(cmp.Or(val, "null")), &d.Create)
		})
	case keyDesktopPass:
		return edit(st, desktopSection, func(d *desktopInVM) error { d.Made.Password = val; return nil })
	case "created":
		if val == "" {
			st.Created = time.Time{}
		} else {
			st.Created = time.Now()
		}
	default:
		panic("no state field " + key)
	}
	return nil
}
