package lima

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Runner runs the commands the backend makes. Tests swap in a fake.
type Runner interface {
	// Run runs a command into the run log, with step on the status line.
	Run(ctx context.Context, step msg.M, cmd *exec.Cmd) error
	// Output runs a command and returns its standard output.
	Output(cmd *exec.Cmd) ([]byte, error)
	// Attach runs a command connected to the streams already set on it.
	Attach(cmd *exec.Cmd) error
}

// logRunner runs commands into the run log through logged (the UI's
// LoggedCmd).
type logRunner struct {
	logged func(ctx context.Context, step msg.M, cmd *exec.Cmd) error
}

func (r logRunner) Run(ctx context.Context, step msg.M, cmd *exec.Cmd) error {
	return r.logged(ctx, step, cmd)
}

func (logRunner) Output(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

func (logRunner) Attach(cmd *exec.Cmd) error { return cmd.Run() }

// limactl returns a limactl command on devvm's own Lima home. It runs in
// devvm's data folder: Lima writes lima.REJECTED.yaml into the folder it
// runs in when a config is invalid, and a project folder is the VM's to
// write, so the VM could have made that name a link to anything. It runs
// without the host's proxy settings (backend.ClientProxyVars, which Lima
// reads for propagateProxyEnv, each bare, which setEnv removes), and only
// a VM on Lima's own network gets them (Start puts them back for it).
// Lima's hostagent logs a value as it reads it, password and all, when it
// replaces a loopback proxy's address with the guest's gateway, overrides
// the system's or lima.yaml's, or can't parse it; limactl start passes
// those lines on to devvm's run log, which keeps URL passwords out, and
// Start takes them out of the hostagent's own log (redactLog). It carries
// no SSH agent: a command that reaches a VM gets the VM's own (withAgent),
// never whichever devvm started with.
func (l *Lima) limactl(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "limactl", args...) //nolint:gosec // devvm's own arguments
	cmd.Env = setEnv(os.Environ(), append(backend.ClientProxyVars(), "SSH_AUTH_SOCK", "LIMA_HOME="+l.Dirs.Lima)...)
	cmd.Dir = l.Dirs.Data
	if err := os.MkdirAll(l.Dirs.Data, 0o700); err != nil && cmd.Err == nil {
		cmd.Err = err // the command then fails with it
	}
	return cmd
}

// Limactl is devvm limactl: limactl with args, on devvm's own Lima home,
// in devvm's place, so its input, output, terminal and exit code are the
// user's. It runs in devvm's data folder, as devvm's own limactl commands
// do. It carries no SSH agent (limactlEnv). It returns only when it can't
// run limactl.
func Limactl(d paths.Dirs, args []string) error {
	path, err := exec.LookPath("limactl")
	if err != nil {
		return limaMissing()
	}
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		return err
	}
	if err := os.Chdir(d.Data); err != nil {
		return err
	}
	argv := append([]string{"limactl"}, args...)
	return platform.Exec(path, argv, limactlEnv(os.Environ(), d))
}

// limactlEnv is devvm limactl's environment: env on devvm's Lima home,
// without SSH_AUTH_SOCK. Lima's ssh shares one connection per VM
// (ControlMaster auto, ControlPersist yes), and an agent forwarded over it
// is the agent of the ssh that opened it, so a limactl shell that opened
// it with the user's agent would hand that agent to devvm's later
// sessions of a VM whose choice is devvm's own agent or none.
func limactlEnv(env []string, d paths.Dirs) []string {
	return setEnv(env, "SSH_AUTH_SOCK", "LIMA_HOME="+d.Lima)
}

// withAgent gives a limactl command that reaches a VM the VM's SSH agent,
// sock: Lima's ssh forwards it, and the connection it shares with later
// ones does too. "" leaves it none.
func withAgent(cmd *exec.Cmd, sock string) {
	if sock != "" {
		cmd.Env = setEnv(cmd.Env, "SSH_AUTH_SOCK="+sock)
	}
}

// setEnv returns env with each KEY=value in kvs replacing any value KEY
// had. A bare KEY removes it.
func setEnv(env []string, kvs ...string) []string {
	out := slices.Clone(env)
	for _, kv := range kvs {
		key, _, hasValue := strings.Cut(kv, "=")
		out = slices.DeleteFunc(out, func(e string) bool { return strings.HasPrefix(e, key+"=") })
		if hasValue {
			out = append(out, kv)
		}
	}
	return out
}

// edit changes a stopped VM's Lima config, with limactl edit's options.
func (l *Lima) edit(ctx context.Context, vm vmname.VM, opts ...string) error {
	args := append([]string{"edit", "--tty=false", vm.String()}, opts...)
	if err := l.Runner.Run(ctx, msg.Configuring(shown(ctx, vm)), l.limactl(ctx, args...)); err != nil {
		return backend.Logged(msg.LimaEditFailed(vm))
	}
	return nil
}

// instance is a VM as limactl list --json has it: the fields devvm reads.
type instance struct {
	Name          vmname.VM `json:"name"` // as vmname.ParseListed reads it
	Status        string    `json:"status"`
	CPUs          int       `json:"cpus"`
	Memory        int64     `json:"memory"` // bytes
	Disk          int64     `json:"disk"`   // bytes
	SSHLocalPort  int       `json:"sshLocalPort"`
	SSHConfigFile string    `json:"sshConfigFile"`
	// VMType is its driver: vz (Virtualization.framework) or qemu. A
	// clone has its image's.
	VMType string `json:"vmType"`
	// HostAgentPID and DriverPID are its hostagent's and its driver's
	// (QEMU's; on vz, the hostagent's own), 0 for one that doesn't run.
	HostAgentPID int     `json:"hostAgentPID"`
	DriverPID    int     `json:"driverPID"`
	Config       *config `json:"config"`
}

// config is the parts of a VM's Lima config devvm reads.
type config struct {
	Mounts            []mount           `json:"mounts"`
	MountType         string            `json:"mountType"`
	Networks          []json.RawMessage `json:"networks"`
	Env               map[string]string `json:"env"`
	PropagateProxyEnv *bool             `json:"propagateProxyEnv"`
	DNS               []string          `json:"dns"`
	PortForwards      []portForward     `json:"portForwards"`
	HostResolver      struct {
		Enabled *bool `json:"enabled"`
	} `json:"hostResolver"`
	SSH struct {
		LocalPort    int   `json:"localPort"`
		ForwardAgent *bool `json:"forwardAgent"`
	} `json:"ssh"`
}

// mount is one of a VM's mounts, as Lima's config and --set have it: the
// fields devvm sets, and the rest as limactl list has them (sshfs's
// settings, 9p's but its cache), which a rewrite of the mounts keeps.
type mount struct {
	Location   string
	MountPoint *string
	Writable   *bool
	// NineP is its 9p settings, Lima's 9p: (QEMU's mounts on a Linux
	// host); nil for none.
	NineP *ninep
	rest  fields
}

// ninep is a mount's 9p settings: its cache mode, which devvm sets
// (backend.Want.ProjectCache), and the rest as they were.
type ninep struct {
	Cache string
	rest  fields
}

// fields is a JSON object's fields that devvm doesn't read, as they were.
type fields map[string]json.RawMessage

// UnmarshalJSON reads a mount as limactl list has it, keeping the fields
// devvm doesn't read but those limactl filled in at Lima's defaults
// (unfilled).
func (m *mount) UnmarshalJSON(data []byte) error {
	*m = mount{}
	var err error
	m.rest, err = decodeObject(data, map[string]any{
		"location": &m.Location, "mountPoint": &m.MountPoint, "writable": &m.Writable, "9p": &m.NineP,
	})
	*m = m.unfilled()
	return err
}

// MarshalJSON writes the mount: what devvm sets, then what it kept.
func (m mount) MarshalJSON() ([]byte, error) {
	set := []field{{"location", m.Location}}
	if m.MountPoint != nil {
		set = append(set, field{"mountPoint", *m.MountPoint})
	}
	if m.Writable != nil {
		set = append(set, field{"writable", *m.Writable})
	}
	if m.NineP != nil {
		set = append(set, field{"9p", m.NineP})
	}
	return encodeObject(set, m.rest)
}

// UnmarshalJSON reads 9p's settings, keeping those devvm doesn't read.
func (n *ninep) UnmarshalJSON(data []byte) error {
	*n = ninep{}
	var err error
	n.rest, err = decodeObject(data, map[string]any{"cache": &n.Cache})
	return err
}

// MarshalJSON writes 9p's settings: the cache mode where it's set, and
// what was kept.
func (n ninep) MarshalJSON() ([]byte, error) {
	var set []field
	if n.Cache != "" {
		set = append(set, field{"cache", n.Cache})
	}
	return encodeObject(set, n.rest)
}

// decodeObject reads data, a JSON object, into known's fields, each by
// its name, and returns the others.
func decodeObject(data []byte, known map[string]any) (fields, error) {
	var all fields
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for k, p := range known {
		if raw, ok := all[k]; ok {
			if err := json.Unmarshal(raw, p); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			delete(all, k)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// field is one of a JSON object's fields that devvm sets.
type field struct {
	name  string
	value any
}

// encodeObject writes set's fields, in order, then rest's, by name, as
// one JSON object.
func encodeObject(set []field, rest fields) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	put := func(k string, v any) error {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		data, err := json.Marshal(map[string]any{k: v})
		if err != nil {
			return err
		}
		b.Write(data[1 : len(data)-1])
		return nil
	}
	for _, f := range set {
		if err := put(f.name, f.value); err != nil {
			return nil, err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(rest)) {
		if err := put(k, rest[k]); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (m mount) writable() bool { return m.Writable != nil && *m.Writable }

// cache is the mount's 9p cache mode, "" for none set.
func (m mount) cache() string {
	if m.NineP == nil {
		return ""
	}
	return m.NineP.Cache
}

// cacheOr is the mount's 9p cache mode, Lima's default for it where it
// sets none: mmap for a writable mount, fscache for a read-only one.
func (m mount) cacheOr() string { return cmp.Or(m.cache(), defaultCache(m.writable())) }

// defaultCache is Lima's 9p cache mode for a mount that sets none.
func defaultCache(writable bool) string {
	if writable {
		return backend.CacheMmap
	}
	return "fscache"
}

// limaFilled are the mount settings limactl list fills in where a VM's
// lima.yaml sets none, by their JSON (Lima 2's, as testdata/golden has
// them), but 9p's cache, whose default is the mount's (cacheOr).
func limaFilled() map[string]map[string]string {
	return map[string]map[string]string{
		"9p":    {"securityModel": `"none"`, "protocolVersion": `"9p2000.L"`, "msize": `"128KiB"`},
		"sshfs": {"cache": `true`, "followSymlinks": `false`, "sftpDriver": `""`},
	}
}

// unfilled is m with only the settings devvm knows (limaFilled's) that
// differ from Lima's defaults, and its own: kept through a rewrite, a
// setting limactl list filled in would be written into lima.yaml, where a
// later Lima's other default would never reach it. A known setting of
// another value is the VM's own, and stays. A setting devvm doesn't know,
// such as one a later Lima adds, goes, which Lima then fills in: devvm
// can't tell its default from a value given (it can't read lima.yaml,
// which is YAML), and keeping it would freeze its default. So does a
// known one at its default value, which Lima then fills in the same. A
// change of a known default in a later Lima (a new golden fixture) means
// a new limaFilled: until then, a VM's rewrite keeps that setting at the
// old default.
func (m mount) unfilled() mount {
	filled := limaFilled()
	var rest fields
	if raw, ok := m.rest["sshfs"]; ok {
		if kept := differing(raw, filled["sshfs"]); kept != nil {
			rest = fields{"sshfs": kept}
		}
	}
	m.rest = rest
	if m.NineP != nil {
		n := ninep{Cache: m.NineP.Cache}
		if n.Cache == defaultCache(m.writable()) {
			n.Cache = ""
		}
		for k, v := range filled["9p"] {
			if raw, ok := m.NineP.rest[k]; ok && compactJSON(raw) != v {
				if n.rest == nil {
					n.rest = fields{}
				}
				n.rest[k] = raw
			}
		}
		m.NineP = &n
		if n.Cache == "" && len(n.rest) == 0 {
			m.NineP = nil
		}
	}
	return m
}

// differing is raw, a JSON object, with only its fields that filled names
// at another value than filled's; nil when none is, or raw isn't an
// object.
func differing(raw json.RawMessage, filled map[string]string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	kept := fields{}
	for k, v := range filled {
		if f, ok := obj[k]; ok && compactJSON(f) != v {
			kept[k] = f
		}
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := encodeObject(nil, kept)
	if err != nil {
		return nil
	}
	return out
}

// compactJSON is raw without its spaces, for comparing values.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// over is m in place of had, the mount it replaces (the project folder's,
// moved or with another cache mode): had's settings that devvm doesn't
// set are kept, and its cache mode too where m sets none.
func (m mount) over(had mount) mount {
	m.rest = had.rest
	if had.NineP != nil {
		n := *had.NineP
		if m.NineP != nil {
			n.Cache = m.NineP.Cache
		}
		m.NineP = &n
	}
	return m
}

// agentDir reports whether the mount is an agent folder: read-only, at the
// same path as on the host (the tools share is elsewhere).
func (m mount) agentDir() bool {
	return !m.writable() && m.atLocation()
}

// project reports whether the mount is the project folder: the one
// read-write mount, at the same path as on the host, or by its device and
// inode at its path in the VM (projectMount).
func (m mount) project() bool { return m.writable() && (m.atLocation() || m.byInode()) }

// atLocation reports whether the mount is at its host path in the VM.
func (m mount) atLocation() bool {
	return m.MountPoint == nil || *m.MountPoint == "" || *m.MountPoint == m.Location
}

// byInode reports whether the mount's host folder is named by its device
// and inode (macOS's /.vol, platform.VolPath).
func (m mount) byInode() bool { return strings.HasPrefix(m.Location, "/.vol/") }

// at is the mount's path in the VM.
func (m mount) at() string {
	if m.atLocation() {
		return m.Location
	}
	return *m.MountPoint
}

// same reports whether m is o as devvm sets o: the same host folder at the
// same path in the VM, as writable, and with o's 9p cache mode where o
// sets one. limactl list fills in what devvm leaves out (mountPoint,
// sshfs's and 9p's settings), so only those count.
func (m mount) same(o mount) bool {
	return m.Location == o.Location && m.at() == o.at() && m.writable() == o.writable() &&
		(o.cache() == "" || m.cacheOr() == o.cache())
}

// projectMount is the mount of the project folder w asks for, read-write
// at its path in the VM, in, of the VM or its image, as limactl list has
// it. On vz it's by the folder's device and inode as devvm recorded them
// (w.ProjectID, platform.VolPath), which Virtualization.framework opens at
// the VM's start wherever the folder is then, so a link put at its path,
// or above it, is never followed. QEMU's 9p has nothing like it, so
// elsewhere it's by its path, with the cache mode w asks for: 9p's cache
// option, which Lima leaves at mmap for a writable mount.
func projectMount(in instance, w backend.Want) mount {
	m := mount{Location: w.Project, Writable: ptr(true)}
	if vol := platform.VolPath(w.ProjectID); in.VMType == "vz" && vol != "" {
		m = mount{Location: vol, MountPoint: ptr(w.Project), Writable: ptr(true)}
	}
	if w.ProjectCache != "" && in.ninep() {
		m.NineP = &ninep{Cache: w.ProjectCache}
	}
	return m
}

// outMount is a build VM's writable output folder at at in the VM ("" for
// its host path), cached with none: what the build writes reaches the
// host as it writes it (backend.CacheNone). Lima reads 9p's settings only
// for a 9p mount, so on vz they're unused.
func outMount(location, at string) mount {
	m := mount{Location: location, Writable: ptr(true), NineP: &ninep{Cache: backend.CacheNone}}
	if at != "" {
		m.MountPoint = ptr(at)
	}
	return m
}

// ninep reports whether the instance's mounts are 9p's: QEMU's, whose
// mount type Lima defaults to 9p, the only one devvm's VMs have on a Linux
// host.
func (in instance) ninep() bool {
	mt := ""
	if in.Config != nil {
		mt = in.Config.MountType
	}
	return in.VMType == "qemu" && (mt == "" || mt == "9p")
}

// list returns every instance in devvm's Lima home: VMs and images.
func (l *Lima) list(ctx context.Context) ([]instance, error) {
	out, err := l.Runner.Output(l.limactl(ctx, "list", "--json"))
	if err != nil {
		return nil, fmt.Errorf("limactl list: %w", err)
	}
	var ins []instance
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var in instance
		if err := dec.Decode(&in); errors.Is(err, io.EOF) {
			return ins, nil
		} else if err != nil {
			return nil, fmt.Errorf("limactl list: %w", err)
		}
		if in.Config == nil {
			in.Config = &config{}
		}
		ins = append(ins, in)
	}
}

// inspect returns one instance, or an error matching backend.ErrNotFound.
func (l *Lima) inspect(ctx context.Context, name vmname.VM) (instance, error) {
	ins, err := l.list(ctx)
	if err != nil {
		return instance{}, err
	}
	for _, in := range ins {
		if in.Name == name {
			return in, nil
		}
	}
	return instance{}, fmt.Errorf("%s: %w", name, backend.ErrNotFound)
}

// checkGlobalConfigs refuses Lima's global configs in devvm's Lima home:
// Lima merges them into every VM, so a setting meant for other VMs (a
// network, a mount) would reach devvm's.
func (l *Lima) checkGlobalConfigs() error {
	if m, ok := l.globalConfigs(); ok {
		return m
	}
	return nil
}

// globalConfigs returns lima_global_config when devvm's Lima home has
// any of Lima's global configs.
func (l *Lima) globalConfigs() (msg.M, bool) {
	var found []string
	for _, f := range []string{"default.yaml", "override.yaml"} {
		p := filepath.Join(l.Dirs.Lima, "_config", f)
		if _, err := os.Lstat(p); err == nil {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		return msg.LimaGlobalConfig(strings.Join(found, " ")), true
	}
	return msg.M{}, false
}

// yqJSON is v as a yq value for limactl's --set: JSON, which yq reads,
// with every string quoted.
func yqJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // devvm's own values: always encodable
	}
	return strings.TrimSuffix(b.String(), "\n")
}
