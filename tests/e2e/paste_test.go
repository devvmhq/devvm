//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// projectPaste: devvm paste, with stand-in clipboard tools (FakeClipboard,
// as NoDocker stands in for docker), puts the copied image into the
// running VM's paste folder, where its checksum matches, mode 600 in the
// folder of mode 700, and the stand-in copier got the path; devvm paste
// rm --force deletes it and nothing else; and a stopped VM is refused
// before the clipboard is read.
func projectPaste(s *S, w *webVM) {
	s.Section("devvm paste")
	p, vm := w.p, w.vm
	image := []byte("\x89PNG\r\n\x1a\ne2e paste " + time.Now().Format(time.RFC3339Nano) + "\n")
	env, copied, cerr := FakeClipboard(s, image)
	path := ""
	s.Check("devvm paste puts the copied image into the VM's paste folder, mode 600, and copies its path", func() error {
		if cerr != nil {
			return cerr
		}
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "paste", "--json"}, Dir: p, Env: env})
		if err != nil {
			return err
		}
		var out struct {
			Path, Kind, Type string
			Bytes            int
			Copied           bool
		}
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
			return fmt.Errorf("its --json: %w: %s", err, r.Stdout)
		}
		switch {
		case !strings.HasPrefix(out.Path, "/tmp/devvm-paste/paste-") || !strings.HasSuffix(out.Path, ".png"):
			return errors.New("its path: " + out.Path)
		case out.Kind != "image" || out.Type != "image/png" || out.Bytes != len(image) || !out.Copied:
			return fmt.Errorf("its --json: %+v", out)
		}
		path = out.Path
		g, err := s.InVM(vm, "sha256sum "+path+" && stat -c %a "+path+" /tmp/devvm-paste")
		if err != nil {
			return err
		}
		sum := sha256.Sum256(image)
		switch {
		case !strings.Contains(g.Stdout, hex.EncodeToString(sum[:])):
			return errors.New("the VM's file differs: " + g.Stdout)
		case !HasLine(g.Stdout, "600") || !HasLine(g.Stdout, "700"):
			return errors.New("the modes: " + g.Stdout)
		}
		if b, err := os.ReadFile(copied); err != nil || string(b) != path {
			return fmt.Errorf("the path copied: %q: %w", b, err)
		}
		return nil
	})
	s.Check("devvm paste rm --force deletes it and leaves a file of yours in the folder", func() error {
		if path == "" {
			return errors.New("nothing was pasted")
		}
		if _, err := s.InVM(vm, "touch /tmp/devvm-paste/mine"); err != nil {
			return err
		}
		r, err := s.DevvmNames(p, "paste", "rm", "--force")
		if err != nil {
			return err
		}
		if err := said(r, "==> paste_rm_done count=1 dir=/tmp/devvm-paste vm="+vm); err != nil {
			return err
		}
		_, err = s.InVM(vm, "test ! -e "+path+" && test -e /tmp/devvm-paste/mine && rm /tmp/devvm-paste/mine")
		return err
	})
	s.Check("a stopped VM is refused before the clipboard is read", func() error {
		if _, err := s.Devvm(p, "stop"); err != nil {
			return err
		}
		r, err := s.DevvmNames(p, "paste")
		if err == nil {
			return errors.New("devvm paste ran on a stopped VM")
		}
		if err := said(r, "devvm: vm_not_running vm="+vm); err != nil {
			return errors.Join(err, errorsOf(r))
		}
		// The folder's VM runs again, as the rest of the section has it.
		_, err = DevvmIn(s, p, "echo e2e-paste-restart\n", nil)
		return err
	})
}

// FakeClipboard is an environment whose test build runs stand-in
// clipboard tools (DEVVM_CLIP_TOOLS), which hold image as the clipboard's
// one image: wl-paste and wl-copy (Wayland, which the environment names),
// and osascript and pbcopy (macOS). Whatever is copied goes to the file
// returned.
func FakeClipboard(s *S, image []byte) (env []string, copied string, err error) {
	dir := filepath.Join(s.sb.work, "clipboard")
	copied = filepath.Join(dir, "copied")
	madeFile(s.sb, dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	img := filepath.Join(dir, "image.png")
	if err := os.WriteFile(img, image, 0o600); err != nil {
		return nil, "", err
	}
	scripts := map[string]string{
		"wl-paste": "#!/bin/sh\ncase \"$*\" in\n*--list-types*) echo text/html; echo image/png ;;\n*image/png*) cat '" + img + "' ;;\n*) echo 'No suitable type of content copied' >&2; exit 1 ;;\nesac\n",
		"wl-copy":  "#!/bin/sh\ncat > '" + copied + "'\n",
		"pbcopy":   "#!/bin/sh\ncat > '" + copied + "'\n",
		// devvm paste's script: -l JavaScript -e SCRIPT FILE LIMIT, the image
		// written to FILE, and its type printed.
		"osascript": "#!/bin/sh\ncase \"$*\" in\n*NSPasteboard*) cp '" + img + "' \"$5\" && echo '{\"type\":\"image/png\"}' ;;\n*) exit 1 ;;\nesac\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the stand-ins run
			return nil, "", err
		}
	}
	return []string{"DEVVM_CLIP_TOOLS=" + dir, "WAYLAND_DISPLAY=wayland-0", "DISPLAY="}, copied, nil
}
