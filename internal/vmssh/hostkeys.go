package vmssh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A VM's host keys are pinned once for each of its starts, in one record
// in the VM's run folder (paths.VM.KnownHosts): a known hosts file whose
// lines are the keys under the VM's name, after a comment naming the
// start they were read for (backend.Run.ID). devvm's ssh config, its port
// forwarder, links' sessions and a --clone VM's git all check the VM's
// sshd against it (UserKnownHostsFile, HostKeyAlias the VM's name). The
// keys are read from the running VM through its backend, which reaches
// the VM itself, never over the network.

// ErrNoHostKeys is a VM whose host keys devvm can't pin for its current
// start: it isn't running, its backend can't tell its start, or it shows
// none.
var ErrNoHostKeys = errors.New("no host keys pinned")

// hostKeyTimeout is how long a VM gets to show its host keys.
const hostKeyTimeout = 30 * time.Second

// hostKeysMax is the most of a VM's answer read for its host keys.
const hostKeysMax = 64 << 10

// hostKeyScript shows a VM's public host keys.
const hostKeyScript = "cat /etc/ssh/ssh_host_*_key.pub"

// startLine is the record's first line, naming the start its keys are of.
const startLine = "# devvm: host keys of start "

// HostKey is one of a VM's ssh host keys: its type and its key, base64.
type HostKey struct{ Type, Key string }

// keyType is a key type's name as ssh has it (ssh-ed25519,
// ecdsa-sha2-nistp256, sk-ssh-ed25519@openssh.com, ssh-rsa…): no
// whitespace, comma or marker (@cert-authority, @revoked), nor a
// pattern's characters.
var keyType = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+-]{0,63}$`)

// keyMax is the longest key, decoded, that a host key line may have:
// above any RSA key ssh makes (16384 bits).
const keyMax = 4 << 10

// ParseHostKeys returns the host keys in out, a VM's public host key
// files (TYPE BASE64 [COMMENT] lines), every key type ssh has. A line is a
// key only when its key decodes, names its own type and has more after
// it, as ssh's wire format does, and isn't a certificate (which a known
// hosts file holds only under @cert-authority); anything else is dropped,
// so no line of the VM's lands in the record as anything but a key.
func ParseHostKeys(out []byte) []HostKey {
	var keys []HostKey
	s := bufio.NewScanner(bytes.NewReader(out))
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 || !keyType.MatchString(f[0]) || strings.Contains(f[0], "-cert-") {
			continue
		}
		blob, err := base64.StdEncoding.Strict().DecodeString(f[1])
		if err != nil || len(blob) > keyMax || len(blob) < 4 {
			continue
		}
		n := binary.BigEndian.Uint32(blob)
		if uint64(n)+4 >= uint64(len(blob)) || string(blob[4:4+n]) != f[0] {
			continue
		}
		keys = append(keys, HostKey{Type: f[0], Key: f[1]})
	}
	return keys
}

// PinHostKeys returns the VM's record of its host keys for its current
// start (KnownHostsFor), reading them from the running VM through b first
// when the record is another start's, or there's none: at most hostKeysMax
// of its answer, within hostKeyTimeout. Keys read while the VM started
// again aren't recorded. ErrNoHostKeys says none can be pinned. Two
// devvms pinning one VM at once write the same record, each whole.
func PinHostKeys(ctx context.Context, d paths.Dirs, b backend.Backend, vm vmname.VM) (string, error) {
	path := d.VM(vm).KnownHosts()
	start, err := currentStart(ctx, b, vm)
	if err != nil {
		return "", err
	}
	if rec, err := recordedStart(path); err != nil {
		return "", err
	} else if rec == start {
		return path, nil
	}
	kctx, cancel := context.WithTimeout(ctx, hostKeyTimeout)
	defer cancel()
	out := &capped{max: hostKeysMax}
	err = b.Exec(kctx, vm, []string{"sh", "-c", hostKeyScript}, backend.ExecOpts{Stdout: out})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w: %w", vm, ErrNoHostKeys, err)
	}
	keys := ParseHostKeys(out.Bytes())
	if len(keys) == 0 {
		return "", fmt.Errorf("%s: %w: it shows none", vm, ErrNoHostKeys)
	}
	if after, err := currentStart(ctx, b, vm); err != nil {
		return "", err
	} else if after != start {
		return "", fmt.Errorf("%s: %w: it started again", vm, ErrNoHostKeys)
	}
	var rec strings.Builder
	rec.WriteString(startLine + start + "\n")
	for _, k := range keys {
		rec.WriteString(vm.String() + " " + k.Type + " " + k.Key + "\n")
	}
	if err := state.WriteFile(path, []byte(rec.String())); err != nil {
		return "", err
	}
	return path, nil
}

// KnownHostsFor returns the VM's record of its host keys when it's the
// one for its current start, without reading them from the VM.
func KnownHostsFor(ctx context.Context, d paths.Dirs, b backend.Backend, vm vmname.VM) (string, error) {
	path := d.VM(vm).KnownHosts()
	start, err := currentStart(ctx, b, vm)
	if err != nil {
		return "", err
	}
	if rec, err := recordedStart(path); err != nil {
		return "", err
	} else if rec != start {
		return "", fmt.Errorf("%s: %w for its start", vm, ErrNoHostKeys)
	}
	return path, nil
}

// ForgetHostKeys removes the VM's record, before a start: a VM whose
// backend config changed can get new host keys when it boots.
func ForgetHostKeys(d paths.Dirs, vm vmname.VM) error {
	if err := os.Remove(d.VM(vm).KnownHosts()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// currentStart is the running VM's start (backend.Run.ID), one line.
func currentStart(ctx context.Context, b backend.Backend, vm vmname.VM) (string, error) {
	run, err := b.Running(ctx, vm)
	if err != nil {
		return "", fmt.Errorf("%s: %w: %w", vm, ErrNoHostKeys, err)
	}
	if run.ID == "" || strings.ContainsAny(run.ID, "\r\n") {
		return "", fmt.Errorf("%s: %w: its start isn't known", vm, ErrNoHostKeys)
	}
	return run.ID, nil
}

// recordedStart is the start the record at path is of: "" for none, or
// a file that isn't a record.
func recordedStart(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // devvm's own file
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(string(data), "\n")
	start, _ := strings.CutPrefix(first, startLine)
	if start == first {
		return "", nil
	}
	return start, nil
}

// capped keeps the first max bytes written to it, and takes the rest
// without keeping it. Its buffer is a field, not embedded, so it has no
// ReadFrom, which io.Copy (os/exec's copy of a command's output) would
// read through without end.
type capped struct {
	buf bytes.Buffer
	max int
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Bytes and String are what it kept.
func (w *capped) Bytes() []byte  { return w.buf.Bytes() }
func (w *capped) String() string { return w.buf.String() }
