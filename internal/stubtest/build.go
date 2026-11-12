package stubtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// stubPackage is the stubs' binary.
const stubPackage = "github.com/devvmhq/devvm/internal/stubtest/stub"

// sources are the folders the stub binary is built from, from the
// module's root.
func sources() []string {
	return []string{"internal/stubtest/stub", "internal/stubtest/stubstate"}
}

// stubCache is the test process's stubs' folder.
var stubCache = sync.OnceValue(func() *cache { return &cache{} })

// A cache is the stubs' folder once found or built.
type cache struct {
	mu  sync.Mutex
	dir string
}

// built is the stubs' folder: the binary and its links, in
// $TMPDIR/devvm-stubs-<hash>, the hash of its sources and Go's version.
// Every test process, and every run until the sources change, finds it
// there, so the binary builds once; folders a day old that another
// build left are removed. One that's gone (a test's TMPDIR) is built
// again.
func built() (string, error) {
	c := stubCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dir != "" && exists(filepath.Join(c.dir, "stub")) {
		return c.dir, nil
	}
	dir, err := build()
	if err == nil {
		c.dir = dir
	}
	return dir, err
}

// build builds the stubs' folder, unless it's there already.
func build() (string, error) {
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	sum, err := sourcesHash(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), "devvm-stubs-"+sum)
	if _, err := os.Stat(filepath.Join(dir, "stub")); err == nil {
		return dir, nil
	}
	tmp, err := os.MkdirTemp(os.TempDir(), "devvm-stubs-new.")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := buildInto(root, tmp); err != nil {
		return "", err
	}
	// Another test process may have built it meanwhile: its folder stays.
	if err := os.Rename(tmp, dir); err != nil && !exists(filepath.Join(dir, "stub")) {
		return "", err
	}
	prune(dir)
	return dir, nil
}

// buildInto builds the binary into dir, then links each stub's name to
// it there.
func buildInto(root, dir string) error {
	ctx := context.Background() //nolint:forbidigo // built once for the whole test process, past any one test
	bin := filepath.Join(dir, "stub")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, stubPackage) //nolint:gosec // the test's own temporary folder
	cmd.Dir = root
	cmd.Env = append(GitEnv(os.Environ()), "CGO_ENABLED=0") // go reads the checkout's own repo to stamp the binary
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building the stubs: %w\n%s", err, out)
	}
	out, err := exec.CommandContext(ctx, bin, "names").Output() //nolint:gosec // the stub just built
	if err != nil {
		return fmt.Errorf("the stubs' names: %w", err)
	}
	for name := range strings.FieldsSeq(string(out)) {
		if name == "stub" {
			continue
		}
		if err := os.Symlink("stub", filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// sourcesHash is the hash of the stub's Go files (not its tests), Go's
// version and the platform, cut to 16 hex digits.
func sourcesHash(root string) (string, error) {
	h := sha256.New()
	h.Write([]byte(runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + "\n"))
	for _, src := range sources() {
		files, err := filepath.Glob(filepath.Join(root, src, "*.go"))
		if err != nil {
			return "", err
		}
		slices.Sort(files)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f) //nolint:gosec // the repo's own sources
			if err != nil {
				return "", err
			}
			h.Write([]byte(f[len(root):] + "\n"))
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// prune removes the stubs' folders other than keep that are a day old.
func prune(keep string) {
	old, _ := filepath.Glob(filepath.Join(os.TempDir(), "devvm-stubs-*"))
	for _, d := range old {
		if fi, err := os.Stat(d); d != keep && err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(d)
		}
	}
}

// moduleRoot is the module's root, found from the test's own folder up.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if exists(filepath.Join(dir, "go.mod")) {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("stubtest: no go.mod above the test's folder")
		}
		dir = up
	}
}

// exists reports whether path is there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
