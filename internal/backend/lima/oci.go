package lima

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// OCIVM is the throwaway VM a container image is converted in (--from).
const OCIVM vmname.VM = "devvm-oci-build"

// convertedSize is the converted disk's size; Lima grows it to base's
// (backend.BaseDiskGiB, base's origin's disk).
const convertedSize = "20G"

// ociSteps is the conversion's steps: the build VM's create, configure
// and start, the conversion, and its delete.
const ociSteps = 5

// converted is a container image converted into what Lima boots
// directly, in a folder of devvm's own (out/vm). done removes the folder
// and lets the next conversion go: once base's VM has started, which
// copies the files in.
type converted struct {
	dir, cmdline, libc string
	glibc              string // for gnu, its glibc version: 2.36
	done               func(context.Context)
}

// convert turns spec.From into a disk, kernel and initrd, without Docker
// (unless only Docker has the image): a throwaway VM from Ubuntu's cloud
// image (o's base.yaml) runs oci/build.sh, o's conversion steps written
// into the conversion's own temporary folder (writeScripts) and mounted
// read-only,
// where crane pulls exactly the recorded digest and bootify.sh
// adds what a container lacks. The image's own code runs as
// root in that VM, so everything it writes is untrusted: it gets only a
// subfolder of devvm's temporary folder, and what comes out is checked
// before Lima sees it. One conversion runs at a time. The build VM and
// the folder go however the run ends. With the user's CAs (trust, a
// build's Trust.Add), the build VM trusts them, and build.sh has the
// image's chroot trust them while bootify.sh runs, with guest/ca.sh
// written and mounted read-only too.
func (l *images) convert(ctx context.Context, f *backend.From, o origin, size backend.Size, trust backend.Script) (*converted, error) {
	if f.Digest == "" {
		return nil, msg.NoDigest(f.Ref)
	}
	if !from.Valid(f.Ref) || !from.Valid(f.Digest) {
		return nil, msg.BadFrom(f.Ref)
	}
	tmp, err := os.MkdirTemp("", "devvm-oci.")
	if err != nil {
		return nil, err
	}
	// Its real path: macOS's temporary folder is under the /var link.
	out, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	var unlock func()
	cleanup := func(ctx context.Context) {
		_ = l.deleteIfThere(ctx, OCIVM)
		_ = os.RemoveAll(out)
		if unlock != nil {
			unlock()
		}
	}
	remove := l.AddCleanup(cleanup)
	vmOut := filepath.Join(out, "vm")
	if err := os.Mkdir(vmOut, 0o700); err != nil {
		return nil, err
	}
	// One at a time: each uses OCIVM, and starts by deleting it.
	if unlock, err = l.lockOCI(ctx); err != nil {
		return nil, err
	}
	ref := f.Digest
	if !strings.Contains(ref, "@") {
		// Docker's ID: an image only Docker has.
		if ref, err = l.exportDocker(ctx, f, vmOut); err != nil {
			return nil, err
		}
	}

	l.Info(msg.ConvertingInVM(f.Ref, OCIVM))
	if err := l.deleteIfThere(ctx, OCIVM); err != nil {
		return nil, err
	}
	oci, guest, err := writeScripts(l.Assets, o.conversion, out, len(trust.Data) > 0)
	if err != nil {
		return nil, err
	}
	ms := []mount{outMount(vmOut, ""), {Location: oci, Writable: ptr(false)}}
	if len(trust.Data) > 0 {
		ms = append(ms, mount{Location: guest, Writable: ptr(false)})
	}
	mounts, err := mountsSet(ms)
	if err != nil {
		return nil, err
	}
	if _, err := l.createFromUbuntu(ctx, OCIVM, o.yaml, size, "--set", mounts); err != nil {
		return nil, err
	}
	// crane pulls from the registry, behind a company proxy through a
	// proxy of devvm's (buildproxy.go).
	conf, hp, proxyDone, err := buildProxy(ctx, l.Lima, OCIVM, backend.NetInternet)
	if err != nil {
		return nil, err
	}
	if err := l.start(ctx, OCIVM, nil, conf); err != nil {
		return nil, err
	}
	if err := l.runScript(ctx, OCIVM, trust, true); err != nil {
		return nil, err
	}
	// build.sh's step's arguments, as given, after the folders; the
	// digest is "-" for Docker's export.
	script := filepath.Join(oci, "build.sh")
	args := slices.Concat([]string{"shell", "--workdir", "/", OCIVM.String(), "sudo", "bash", script, oci, vmOut, ref}, o.conversion[0].Args[1:])
	if len(trust.Data) > 0 {
		args = append(args, filepath.Join(guest, "ca.sh"))
	}
	cmd := l.limactl(ctx, args...)
	// With the build's proxy, its variables come first on the input, and
	// the script runs with its arguments after them.
	if pre := proxyExports(hp); pre != nil {
		at := slices.Index(args, "bash") + 1
		cmd = l.limactl(ctx, slices.Concat(args[:at], []string{"-s", "--"}, args[at:])...)
		cmd.Stdin = bytes.NewReader(append(pre, "exec bash \"$@\"\n"...))
	}
	if err := l.Runner.Run(ctx, msg.Converting(f.Ref), cmd); err != nil {
		return nil, backend.Logged(msg.ConvertFailed(f.Ref))
	}
	if err := l.delete(ctx, OCIVM); err != nil {
		return nil, err
	}
	proxyDone(true)
	// The build VM is gone, so nothing changes them now.
	c, err := checkConverted(vmOut, f.Ref)
	if err != nil {
		return nil, err
	}
	c.done = func(ctx context.Context) {
		cleanup(ctx)
		remove()
	}
	return c, nil
}

// conversionScripts are devvm's scripts that convert a --from image, in
// oci/: build.sh runs the others.
var conversionScripts = []string{"oci/build.sh", "oci/bootify.sh", "oci/kernel.sh"}

// convertArgs is how many arguments build.sh's step has (convertSteps).
const convertArgs = 6

// convertSteps are the conversion's origin steps for f, from devvm's
// files: build.sh after the prelude (image.OwnScript), run as root with
// what it makes of the image as its arguments (f's digest and reference, the host's user as the image's
// user, its name, uid and home, and the converted disk's size), then the
// scripts it runs. The conversion runs these as they are.
func (l *images) convertSteps(f *backend.From) ([]image.Step, error) {
	u, err := l.User()
	if err != nil {
		return nil, err
	}
	steps := make([]image.Step, len(conversionScripts))
	for i, name := range conversionScripts {
		// build.sh runs with bash, after the prelude; bootify.sh runs
		// with the image's own sh, and kernel.sh uses none of it.
		read := fs.ReadFile
		if i == 0 {
			read = image.OwnScript
		}
		if steps[i].Data, err = read(l.Assets, name); err != nil {
			return nil, err
		}
		steps[i].Name = name
	}
	steps[0].Root = true
	steps[0].Args = []string{f.Digest, f.Ref, u.Username, u.Uid, "/home/" + u.Username + ".guest", convertedSize}
	return steps, nil
}

// writeScripts writes the conversion's steps (convertSteps) into out, the
// conversion's own temporary folder, and with the user's CAs (ca, a
// build's Trust.Add), devvm's guest/ca.sh from own, beside vm/ rather
// than in it, as the build VM writes vm/. Files are the user's alone
// (state.WriteFile). It returns the two folders, which the build VM
// mounts read-only; the conversion's cleanup deletes them with out.
func writeScripts(own fs.FS, steps []image.Step, out string, cas bool) (oci, guest string, err error) {
	files := slices.Clone(steps)
	if cas {
		ca, err := fs.ReadFile(own, "guest/ca.sh")
		if err != nil {
			return "", "", err
		}
		files = append(files, image.Step{Name: "guest/ca.sh", Data: ca})
	}
	for _, f := range files {
		if err := state.WriteFile(filepath.Join(out, filepath.FromSlash(f.Name)), f.Data); err != nil {
			return "", "", err
		}
	}
	return filepath.Join(out, "oci"), filepath.Join(out, "guest"), nil
}

// exportDocker saves an image only Docker has into dir, as image.tar,
// which build.sh reads for "-". The export must be the image looked up:
// it's saved by name, so the tag may have moved since.
func (l *images) exportDocker(ctx context.Context, f *backend.From, dir string) (string, error) {
	if !l.DockerRunning(ctx) {
		return "", msg.DockerOnly(f.Ref)
	}
	l.Info(msg.ExportingDocker(f.Ref))
	file := filepath.Join(dir, "image.tar")
	cmd := exec.CommandContext(ctx, "docker", "save", "-o", file, f.Ref) //nolint:gosec // a checked reference
	if err := l.Runner.Run(ctx, msg.ExportingDocker(f.Ref), cmd); err != nil {
		return "", backend.Logged(msg.DockerExportFailed(f.Ref))
	}
	if !exportIs(file, f.Digest) {
		return "", msg.DockerImageChanged(f.Ref)
	}
	return "-", nil
}

// maxExportJSON caps what's read of an export's manifest.json and
// index.json: a few KB.
const maxExportJSON = 16 << 20

// exportIs reports whether a docker save tarball holds the image with
// Docker ID id: its config, as Docker's own image store names it, or its
// manifest, as the containerd image store does.
func exportIs(file, id string) bool {
	if !from.IsDigest(id) {
		return false
	}
	h := strings.TrimPrefix(id, "sha256:")
	fh, err := os.Open(file) //nolint:gosec // devvm's own temporary folder
	if err != nil {
		return false
	}
	defer func() { _ = fh.Close() }()
	var manifest []struct{ Config string }
	var index struct {
		Manifests []struct{ Digest string }
	}
	tr := tar.NewReader(fh)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		var dst any
		switch hdr.Name {
		case "manifest.json":
			dst = &manifest
		case "index.json":
			dst = &index
		default:
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxExportJSON))
		if err == nil {
			_ = json.Unmarshal(data, dst)
		}
	}
	for _, m := range manifest {
		if m.Config == "blobs/sha256/"+h || m.Config == h+".json" {
			return true
		}
	}
	for _, m := range index.Manifests {
		if m.Digest == id {
			return true
		}
	}
	return false
}

var cmdlinePattern = regexp.MustCompile(`^[A-Za-z0-9=,./:_ -]+$`)

// maxSmallOutput caps what's read of the conversion's text files.
const maxSmallOutput = 4096

// checkConverted checks what the conversion wrote into dir, and returns
// it: its libc, its glibc version for gnu, and its kernel command line.
// Each file must be a regular file (a link could point Lima or devvm at a
// host file); what goes into Lima's config or base's record must be plain
// text of its shape; the disk must be raw ext4, as build.sh writes it
// (another format, qcow2 or VMDK, can name host files as its backing file
// or extents).
func checkConverted(dir, ref string) (*converted, error) {
	bad := func(file string) error { return msg.ConvertBadOutput(file, ref) }
	regular := func(f string) bool {
		fi, err := os.Lstat(filepath.Join(dir, f))
		return err == nil && fi.Mode().IsRegular()
	}
	for _, f := range []string{"disk.raw", "kernel", "initrd", "cmdline", "libc"} {
		if !regular(f) {
			return nil, bad(f)
		}
	}
	text := func(f string) string {
		fh, err := os.Open(filepath.Join(dir, f)) //nolint:gosec // devvm's own temporary folder
		if err != nil {
			return ""
		}
		defer func() { _ = fh.Close() }()
		data, _ := io.ReadAll(io.LimitReader(fh, maxSmallOutput))
		return strings.TrimRight(string(data), "\n")
	}
	c := &converted{dir: dir}
	if c.libc = text("libc"); c.libc != "gnu" && c.libc != "musl" {
		return nil, bad("libc")
	}
	if c.libc == "gnu" {
		// Checked a plain file before it's read: a link could name a host
		// FIFO, whose read would wait.
		if !regular("glibc") {
			return nil, bad("glibc")
		}
		if c.glibc = text("glibc"); !image.ValidGlibc(c.glibc) {
			return nil, bad("glibc")
		}
	}
	if c.cmdline = text("cmdline"); !cmdlinePattern.MatchString(c.cmdline) {
		return nil, bad("cmdline")
	}
	if !ext4(filepath.Join(dir, "disk.raw")) {
		return nil, bad("disk.raw")
	}
	return c, nil
}

// ext4 reports whether a file is an ext4 filesystem as mke2fs writes one
// into a new file: its first 1024 bytes zero, then a superblock with its
// magic, 0xEF53, at byte 1080. Another format's header (qcow2's, VMDK's)
// is in those first bytes, and could otherwise sit before a superblock.
func ext4(path string) bool {
	fh, err := os.Open(path) //nolint:gosec // devvm's own temporary folder
	if err != nil {
		return false
	}
	defer func() { _ = fh.Close() }()
	head := make([]byte, 1082)
	if _, err := io.ReadFull(fh, head); err != nil {
		return false
	}
	return !slices.ContainsFunc(head[:1024], func(b byte) bool { return b != 0 }) && bytes.Equal(head[1080:], []byte{0x53, 0xef})
}

// lockOCI takes the conversion lock, an OS lock on a file in devvm's data
// folder, until unlock is called or devvm ends, however it ends. It waits
// for another devvm that holds it, saying so after OCINote.
func (l *images) lockOCI(ctx context.Context) (unlock func(), err error) {
	path := l.Dirs.OCIBuildLock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return platform.LockFileWaiting(ctx, path, l.OCINote, func() { l.Info(msg.OCIWaiting()) })
}
