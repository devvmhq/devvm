package paths

import "path/filepath"

// VM is the folder of one VM's files (Dirs.VM), and each file in it:
//
//	vm.json   its state: what it was made from and asked for, and what
//	          devvm made for it (state.VM)
//	lock      held by each change to vm.json
//	policy    its own network rules
//	run/      what its runs leave: pids, its proxy's files and CA, its
//	          port forwarder's, its pinned host keys, its sandbox
//	          profile, its secrets' bindings, its links' list
//	logs/     its proxy's decisions and output, its link session's and
//	          its port forwarder's
//	home.tar.gz  its home folder, while a remake makes it again
//
// None of them is a socket, so none has the socket limit (SocketMax):
// the sockets VMs need are in the run folder, by hash.
type VM struct{ Dir string }

// State is the VM's state file, vm.json.
func (v VM) State() string { return filepath.Join(v.Dir, "vm.json") }

// Lock is the lock file each change to the VM's state holds (flock), so
// two devvms changing one VM at once lose neither's change.
func (v VM) Lock() string { return filepath.Join(v.Dir, "lock") }

// Policy holds the VM's own network rules.
func (v VM) Policy() string { return filepath.Join(v.Dir, "policy") }

// Run is the folder of what the VM's runs leave: pids, its proxy's files
// and CA, its port forwarder's, its pinned host keys, its sandbox profile,
// its secrets' bindings and its links' list.
func (v VM) Run() string { return filepath.Join(v.Dir, "run") }

// Logs is the folder of the VM's logs.
func (v VM) Logs() string { return filepath.Join(v.Dir, "logs") }

// Home is the VM's home folder as a remake saved it from its old disk,
// for its new one: there only until the new disk has it.
func (v VM) Home() string { return filepath.Join(v.Dir, "home.tar.gz") }

// RemakeLock is the lock file a devvm holds (flock) while it makes the VM
// again or finishes doing so, so a second devvm never finishes a remake
// that's still running.
func (v VM) RemakeLock() string { return filepath.Join(v.Run(), "remake.lock") }

// ProxyConfig is the config the VM's proxy runs with
// (proxy.Config): everything it started with, its password among it.
func (v VM) ProxyConfig() string { return filepath.Join(v.Run(), "proxy.json") }

// Proxy is a file of the VM's proxy's own, by ext: .pid, its pid file,
// and .status, the status file it writes once it listens.
func (v VM) Proxy(ext string) string { return filepath.Join(v.Run(), "proxy"+ext) }

// GitPins is a file of a Linux host's pins in QEMU's namespace, by ext:
// .jsonl, what the namespace records of the pins it lost and made again
// while the VM ran (a git command on the host replaced a pinned file, a
// hard link), one JSON object a line, which .git protection reports once
// the VM stops; .json, what the start pins, which devvm _qemu reads
// (backend.Pins.File), written private at each start that pins.
func (v VM) GitPins(ext string) string { return filepath.Join(v.Run(), "git-pins"+ext) }

// CA is the folder of the VM's own CA, which its proxy intercepts with
// and which only it trusts.
func (v VM) CA() CA { return CA{Dir: filepath.Join(v.Run(), "ca")} }

// CA is a VM's CA folder (VM.CA) and its files: the CA its proxy signs
// with now and the one after it, which starts before the first ends and
// which the VM trusts too, each a certificate (PEM) and its key. The keys
// never leave the host.
type CA struct{ Dir string }

// Cert is the current CA's certificate, ca.pem.
func (c CA) Cert() string { return filepath.Join(c.Dir, "ca.pem") }

// Key is the current CA's key, ca.key.
func (c CA) Key() string { return filepath.Join(c.Dir, "ca.key") }

// NextCert is the next CA's certificate, next.pem.
func (c CA) NextCert() string { return filepath.Join(c.Dir, "next.pem") }

// NextKey is the next CA's key, next.key.
func (c CA) NextKey() string { return filepath.Join(c.Dir, "next.key") }

// Lock is the lock file each change to the CAs holds (flock), so two
// launches of the VM at once never leave a key and a certificate of
// different runs.
func (c CA) Lock() string { return filepath.Join(c.Dir, "lock") }

// BuildProxy is the folder of a build VM's proxy, behind a company proxy:
// its files and logs, kept when the build fails.
func (v VM) BuildProxy() string { return filepath.Join(v.Run(), "build-proxy") }

// Profile is the sandbox profile Lima's hostagent runs under on macOS.
func (v VM) Profile() string { return filepath.Join(v.Run(), "sandbox.sb") }

// Started records the hostagent the VM was started on, and how it was
// confined.
func (v VM) Started() string { return filepath.Join(v.Run(), "started") }

// Secrets is the bindings of the VM's secrets that its proxy reads.
func (v VM) Secrets() string { return filepath.Join(v.Run(), "secrets.json") }

// SecretsPolicy is the network rules its secrets' hosts add.
func (v VM) SecretsPolicy() string { return filepath.Join(v.Run(), "secrets.policy") }

// LinksPID is the pid file of the VM's link session.
func (v VM) LinksPID() string { return filepath.Join(v.Run(), "links.pid") }

// LinkList is the list of the VM's links that its link session writes
// and its proxy reads.
func (v VM) LinkList() string { return filepath.Join(v.Run(), "links.json") }

// EventLog is the VM's proxy's decisions (JSON lines).
func (v VM) EventLog() string { return filepath.Join(v.Logs(), "network.jsonl") }

// ProxyLog is the VM's proxy's own output.
func (v VM) ProxyLog() string { return filepath.Join(v.Logs(), "proxy.log") }

// LinksLog is the VM's link session's own output.
func (v VM) LinksLog() string { return filepath.Join(v.Logs(), "links.log") }

// Forwarder is one of the files of the VM's port forwarder in Run, named
// by ext: .pid, .status, .lock, and .forwarded, the host ports it
// forwards now.
func (v VM) Forwarder(ext string) string { return filepath.Join(v.Run(), "ports"+ext) }

// KnownHosts is the VM's host keys as devvm pins them for its current
// start (vmssh.PinHostKeys), which every ssh of devvm's to the VM checks.
func (v VM) KnownHosts() string { return filepath.Join(v.Run(), "known_hosts") }

// ForwarderLog is the VM's port forwarder's own output.
func (v VM) ForwarderLog() string { return filepath.Join(v.Logs(), "ports.log") }
