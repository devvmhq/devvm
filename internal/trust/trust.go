// Package trust is the user's own CAs, a company's for a proxy that
// inspects TLS: read from devvm's CA folder (paths.Dirs.TrustedCAs) and
// checked, then trusted in project VMs (sent at each launch, installed by
// guest/ca.sh), in build VMs while they build (never in the images), by
// devvm's proxy for its own connections, and by devvm's mise on the host
// (internal/tools; on Linux with Host's bundle). It also makes each VM's
// own CA (InitCA), which the VM's proxy intercepts with.
package trust

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// Paths in a VM, as guest/ca.sh writes them.
const (
	// Bundle holds the CAs devvm adds, all in one file: the user's and
	// the proxy's. Empty when there are none.
	Bundle = "/etc/ssl/devvm.pem"
	// System is the system's bundle, theirs included (Debian, Ubuntu and
	// Alpine alike).
	System = "/etc/ssl/certs/ca-certificates.crt"
)

// Env is the guest environment that points tools that don't read the
// system's store at it, or at Bundle: Node (one file of extra CAs),
// Python's requests and pip (certifi otherwise), uv, and what reads
// SSL_CERT_FILE (Go, rustls' native roots, OpenSSL).
var Env = map[string]string{
	"NODE_EXTRA_CA_CERTS": Bundle,
	"SSL_CERT_FILE":       System,
	"REQUESTS_CA_BUNDLE":  System,
	"PIP_CERT":            System,
	"UV_SYSTEM_CERTS":     "1",
}

// GuestEnv returns env with Env added: what every project VM starts with,
// CAs or none, so adding one changes nothing a start sets.
func GuestEnv(env map[string]string) map[string]string {
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, Env)
	return out
}

// CAs is the user's CA certificates, each one PEM-encoded from its
// parsed form alone: nothing else of a file is ever sent.
type CAs [][]byte

// Read returns the CA certificates in dir's *.pem and *.crt files, by file
// name, then in their order in the file, each once; none when dir is
// missing or has none. A file holding anything but certificates (a
// private key), one devvm can't parse, or a certificate that isn't a CA's
// (BasicConstraints) fails it: nothing of the folder is used then. So
// does a link, the folder's or a file's: one into a project folder would
// let that project's VM choose a CA every VM, build and proxy trusts. A
// file or folder it can't read fails it too, named.
func Read(dir string) (CAs, error) {
	switch fi, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, msg.CAFolderUnreadable(dir).Because(err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, msg.CAFolderLink(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, msg.CAFolderUnreadable(dir).Because(err)
	}
	var cas CAs
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".pem" && ext != ".crt" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, ok, err := readFile(path)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		got, err := parse(path, data)
		if err != nil {
			return nil, err
		}
		for _, c := range got {
			if !slices.ContainsFunc(cas, func(o []byte) bool { return bytes.Equal(o, c) }) {
				cas = append(cas, c)
			}
		}
	}
	return cas, nil
}

// readFile returns the data of a file in the CA folder, never following a
// link; false for what isn't a file (a folder), which Read skips.
func readFile(path string) ([]byte, bool, error) {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil, false, msg.CAUnreadable(path).Because(err)
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, false, msg.CALink(path)
	case !fi.Mode().IsRegular():
		return nil, false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the user's own CA folder
	if err != nil {
		return nil, false, msg.CAUnreadable(path).Because(err)
	}
	return data, true, nil
}

// parse returns the CA certificates in file's data, PEM again.
func parse(file string, data []byte) (CAs, error) {
	var cas CAs
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, msg.CANotCertificate(file)
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, msg.CAInvalid(file).Because(err)
		}
		if !c.BasicConstraintsValid || !c.IsCA {
			return nil, msg.CANotCA(file)
		}
		cas = append(cas, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	if len(cas) == 0 {
		return nil, msg.CANotCertificate(file)
	}
	return cas, nil
}

// Bundle returns them all in one PEM file; nil for none.
func (c CAs) Bundle() []byte { return bytes.Join(c, nil) }

// Host returns what makes a program on a Linux host that reads its roots
// from files trust them as well as the host's own CAs, as devvm's mise
// does (rustls' native roots): a bundle of both, which the caller writes
// at path, and the environment that points the program at it. The host's
// bundle is the file SSL_CERT_FILE names when getenv has one, else the
// first of hostBundles that's there, and SSL_CERT_FILE then names path.
// Such a program reads the host's folders of CAs (hostDirs) only when
// neither SSL_CERT_FILE nor SSL_CERT_DIR is set, so then SSL_CERT_DIR
// names them. Otherwise it reads what getenv's name, as before: an
// SSL_CERT_DIR of getenv's stays, and an SSL_CERT_FILE alone gets no
// folder, so it trusts nothing it didn't. Nothing for no CAs.
func (c CAs) Host(path string, getenv func(string) string) (bundle []byte, env []string, err error) {
	if len(c) == 0 {
		return nil, nil, nil
	}
	own := getenv("SSL_CERT_FILE")
	probe := own == "" && getenv("SSL_CERT_DIR") == ""
	if own == "" {
		own = firstThere(hostBundles())
	}
	if own != "" {
		data, err := os.ReadFile(own) //nolint:gosec // the host's own CA bundle, as the program would read it
		switch {
		case err == nil:
			bundle = append(bytes.TrimRight(data, "\n"), '\n')
		case !errors.Is(err, fs.ErrNotExist):
			return nil, nil, err
		}
	}
	bundle = append(bundle, c.Bundle()...)
	env = []string{"SSL_CERT_FILE=" + path}
	if probe {
		var dirs []string
		for _, d := range hostDirs() {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) > 0 {
			env = append(env, "SSL_CERT_DIR="+strings.Join(dirs, ":"))
		}
	}
	return bundle, env, nil
}

// hostBundles are where a Linux host's CA bundle may be, in the order
// rustls' native roots look for it (openssl-probe's list): the first
// that's there is the host's.
func hostBundles() []string {
	return []string{
		"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Arch
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // Fedora, RHEL
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem", // openSUSE
		"/etc/pki/tls/cacert.pem",
		"/etc/ssl/cert.pem", // Alpine
		"/opt/etc/ssl/certs/ca-certificates.crt",
		"/etc/ssl/certs/cacert.pem",
	}
}

// hostDirs are the folders of a Linux host's CAs, a file each, that
// rustls' native roots read besides its bundle (openssl-probe's list).
func hostDirs() []string {
	return []string{"/etc/ssl/certs", "/etc/pki/tls/certs", "/etc/security/certificates"}
}

// firstThere is the first of paths that's there, followed if a link; ""
// for none.
func firstThere(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Name is the file the i-th one (from 0) is installed as in a VM.
func Name(i int) string { return "devvm-extra-" + strconv.Itoa(i+1) + ".crt" }

// Build returns the scripts that make a build VM trust them while it
// builds, from devvm's guest/ca.sh: none for no CAs.
func (c CAs) Build(assets fs.FS) (backend.Trust, error) {
	if len(c) == 0 {
		return backend.Trust{}, nil
	}
	ca, err := fs.ReadFile(assets, "guest/ca.sh")
	if err != nil {
		return backend.Trust{}, err
	}
	// Each in a file of its own in a temporary folder, ca.sh's DIR: written
	// from heredocs, whose delimiter no PEM line can be.
	var add bytes.Buffer
	add.WriteString("set -eu\nd=$(mktemp -d)\ntrap 'rm -rf \"$d\"' EXIT\n")
	for i, pemData := range c {
		add.WriteString("cat > \"$d/" + Name(i) + "\" <<'DEVVM_CA'\n")
		add.Write(pemData)
		add.WriteString("DEVVM_CA\n")
	}
	add.WriteString("set -- \"$d\"\n")
	add.Write(ca)
	remove := append([]byte("set --\n"), ca...)
	return backend.Trust{
		Add:    backend.Script{Name: "ca.sh", Data: add.Bytes()},
		Remove: backend.Script{Name: "ca.sh", Data: remove},
		Env:    c.exports(),
	}, nil
}

// exports are the shell lines that export Env, Trust's Env, which the
// backend puts at the start of each of a build's scripts: a build
// script's tools then find the CAs while it runs, and the image keeps
// none of it. None for no CAs.
func (c CAs) exports() []byte {
	if len(c) == 0 {
		return nil
	}
	var b bytes.Buffer
	for _, k := range slices.Sorted(maps.Keys(Env)) {
		b.WriteString("export " + k + "=" + Env[k] + "\n")
	}
	return b.Bytes()
}
