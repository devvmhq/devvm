package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/safefile"
)

// The fixtures are cache.nixos.org's own: Nix 2.35.4 for aarch64-linux, and its NAR.
const (
	fixHash = "yv1zf3y6all4j99aad30cb8i55dw9zmp"
	fixNar  = "nar/0p7ahb9j02av0zx4hqmwqdpbdas9i25ql899mvf9rc01lr85668w.nar.zst"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "nix", filepath.Base(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNixPath(t *testing.T) {
	t.Parallel()
	h32, h52 := strings.Repeat("a", 32), strings.Repeat("0", 52)
	for p, want := range map[string]nixKind{
		"nix-cache-info":                     nixInfo,
		h32 + ".narinfo":                     nixNarinfo,
		"nar/" + h52 + ".nar":                nixNar,
		"nar/" + h52 + ".nar.xz":             nixNar,
		"nar/" + h52 + ".nar.zst":            nixNar,
		"nar/" + h52 + ".nar.bz2":            nixNar,
		"nar/" + h52 + ".nar.gz":             nixNone,
		"nar/" + h52 + ".nar.xz/x":           nixNone,
		"nar/../" + h32 + ".narinfo":         nixNone,
		"../" + h32 + ".narinfo":             nixNone,
		strings.Repeat("e", 32) + ".narinfo": nixNone, // e isn't in Nix's base 32
		h32[:31] + ".narinfo":                nixNone,
		h32 + "a.narinfo":                    nixNone,
		strings.ToUpper(h32) + ".narinfo":    nixNone,
		"nar/" + h52[:51] + ".nar":           nixNone,
		"nar/" + h52 + "0.nar":               nixNone,
		"nix-cache-info/":                    nixNone,
		"":                                   nixNone,
		"log/" + h32:                         nixNone,
		"realisations/x.doi":                 nixNone,
	} {
		if got := nixPath(p); got != want {
			t.Errorf("nixPath(%q) = %d, want %d", p, got, want)
		}
	}
}

func TestNix32(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256(fixture(t, fixNar))
	if got, want := nix32(sum[:]), "1jrm0rjd3i5vxv2sjm6zgqqi87ikb3fjqg3myg7wj0x4q5psb550"; got != want {
		t.Errorf("nix32 = %s, want %s (the narinfo's FileHash)", got, want)
	}
}

func TestParseNarinfo(t *testing.T) {
	t.Parallel()
	good := string(fixture(t, fixHash+".narinfo"))
	ni, err := parseNarinfo([]byte(good), fixHash)
	if err != nil {
		t.Fatalf("cache.nixos.org's own narinfo: %v", err)
	}
	if ni.url != fixNar || ni.fileSize != 958 || ni.fileHash != "sha256:1jrm0rjd3i5vxv2sjm6zgqqi87ikb3fjqg3myg7wj0x4q5psb550" {
		t.Errorf("read %+v", ni)
	}
	swap := func(old, nu string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("the fixture has no %q", old)
		}
		return strings.Replace(good, old, nu, 1)
	}
	sig := regexp.MustCompile(`(?m)^Sig: .*$`).FindString(good)
	for name, bad := range map[string]string{
		"an added reference": swap("References: ", "References: 00000000000000000000000000000000-evil "),
		"another NarHash":    swap("NarHash: sha256:0p7", "NarHash: sha256:1p7"),
		"another NarSize":    swap("NarSize: 8848", "NarSize: 8849"),
		"another store path": swap("-nix-2.35.4\nURL", "-nix-2.35.5\nURL"),
		"no signature":       swap(sig, ""),
		"another key's name": swap("Sig: cache.nixos.org-1:", "Sig: evil-1:"),
		"a broken signature": swap("Sig: cache.nixos.org-1:x", "Sig: cache.nixos.org-1:y"),
		"a URL elsewhere":    swap("URL: nar/", "URL: https://evil.test/nar/"),
		"a URL with a query": swap(".nar.zst\n", ".nar.zst?x=1\n"),
		"a URL up a folder":  swap("URL: nar/", "URL: ../nar/"),
		"another NAR's URL":  swap("URL: nar/0p7", "URL: nar/1p7"),
		"StorePath twice":    good + "StorePath: /nix/store/" + fixHash + "-x\n",
		"FileSize twice":     good + "FileSize: 1\n",
		"no FileSize":        regexp.MustCompile(`(?m)^FileSize: .*\n`).ReplaceAllString(good, ""),
		"a size of 0":        swap("FileSize: 958", "FileSize: 0"),
		"FileHash in hex":    swap("FileHash: sha256:1jrm0rjd3i5vxv2sjm6zgqqi87ikb3fjqg3myg7wj0x4q5psb550", "FileHash: sha256:"+strings.Repeat("ab", 32)),
		"a bad reference":    swap("References: ", "References: ../x "),
	} {
		if _, err := parseNarinfo([]byte(bad), fixHash); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseNarinfo([]byte(good), "00000000000000000000000000000000"); err == nil {
		t.Error("a narinfo for another path: accepted")
	}
	// The signature covers what Nix checks, not the rest: a Deriver or Compression changed still
	// verifies, and the VM's Nix doesn't use them to trust the path.
	if _, err := parseNarinfo([]byte(swap("Deriver: 69w", "Deriver: 79w")), fixHash); err != nil {
		t.Errorf("another Deriver: %v", err)
	}
}

// fakeNixCache is cache.nixos.org as the tests have it: files by path, and every request it got.
type fakeNixCache struct {
	mu    sync.Mutex
	files map[string][]byte
	got   []string
	srv   *httptest.Server
}

func (f *fakeNixCache) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// useNixCache is a proxy whose cache is a temp folder and a fake cache.nixos.org, with policy
// rules.
func useNixCache(t *testing.T, rules string) (*server, *fakeNixCache, string) {
	t.Helper()
	f := &fakeNixCache{files: map[string][]byte{
		"/" + fixHash + ".narinfo": fixture(t, fixHash+".narinfo"),
		"/" + fixNar:               fixture(t, fixNar),
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Proxy-Authorization")+r.Header.Get("Cookie"))
		b, ok := f.files[r.URL.Path]
		f.mu.Unlock()
		if r.URL.Path == "/redirect.narinfo" {
			http.Redirect(w, r, "http://evil.test/x", http.StatusFound)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	dir := filepath.Join(t.TempDir(), "nix-cache")
	s := testServer(t, Config{NixCache: dir}, rules)
	s.nix.base = "http://" + nixHost
	addr := f.srv.Listener.Addr().String()
	s.nix.dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	s.nix.floor = func(context.Context) error { return nil }
	return s, f, dir
}

func nixReq(t *testing.T, s *server, method, target string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	r.Header.Set("Proxy-Authorization", "Basic secret")
	r.Header.Set("Cookie", "vm=1")
	w := httptest.NewRecorder()
	s.serveReserved(w, r)
	return w.Code, w.Body.Bytes()
}

func TestNixCacheServes(t *testing.T) {
	t.Parallel()
	s, f, dir := useNixCache(t, nixHost)
	log := logs(s)
	if code, body := nixReq(t, s, "GET", "http://devvm.proxy/nix/nix-cache-info"); code != 200 || string(body) != nixCacheInfo {
		t.Fatalf("nix-cache-info: %d %q", code, body)
	}
	// A NAR no checked narinfo names isn't fetched.
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixNar); code != 404 || len(f.requests()) != 0 {
		t.Fatalf("a NAR before its narinfo: %d, upstream got %q", code, f.requests())
	}
	narinfo := "http://devvm.proxy/nix/" + fixHash + ".narinfo"
	if code, body := nixReq(t, s, "GET", narinfo); code != 200 || string(body) != string(fixture(t, fixHash+".narinfo")) {
		t.Fatalf("narinfo: %d %q", code, body)
	}
	if code, body := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixNar); code != 200 || string(body) != string(fixture(t, fixNar)) {
		t.Fatalf("NAR: %d, %d bytes", code, len(body))
	}
	// Again, from the cache alone, as another VM's proxy would serve them.
	other := testServer(t, Config{NixCache: dir}, nixHost)
	other.nix.floor = func(context.Context) error { return errors.New("asked cache.nixos.org") }
	for _, p := range []string{fixHash + ".narinfo", fixNar} {
		if code, _ := nixReq(t, other, "GET", "http://devvm.proxy/nix/"+p); code != 200 {
			t.Errorf("%s again: %d", p, code)
		}
	}
	if code, body := nixReq(t, s, "HEAD", narinfo); code != 200 || len(body) != 0 {
		t.Errorf("HEAD: %d %q", code, body)
	}
	got := f.requests()
	want := []string{"GET /" + fixHash + ".narinfo ", "GET /" + fixNar + " "}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("upstream got %q, want %q: nothing of the VM's request, each file once", got, want)
	}
	for _, p := range []string{"", "nar", fixHash + ".narinfo", fixNar} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]os.FileMode{true: 0o700, false: 0o600}[fi.IsDir()]; fi.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", p, fi.Mode().Perm(), want)
		}
	}
	for _, s := range []string{`"event":"nix-cache"`, `"reason":"fetched"`, `"reason":"hit"`, `"reason":"not found: no checked narinfo names it"`} {
		if !strings.Contains(log.String(), s) {
			t.Errorf("the log has no %s:\n%s", s, log)
		}
	}
}

func TestNixCacheRefuses(t *testing.T) {
	t.Parallel()
	s, f, dir := useNixCache(t, nixHost)
	h32 := strings.Repeat("a", 32)
	for _, c := range []struct{ method, target string }{
		{"POST", "http://devvm.proxy/nix/" + fixHash + ".narinfo"},
		{"PUT", "http://devvm.proxy/nix/" + fixNar},
		{"DELETE", "http://devvm.proxy/nix/" + fixNar},
		{"GET", "http://devvm.proxy/nix/" + fixHash + ".narinfo?x=1"},
		{"GET", "http://devvm.proxy/nix/" + fixHash + ".narinfo?"},
		{"GET", "http://devvm.proxy/nix/%79v1zf3y6all4j99aad30cb8i55dw9zmp.narinfo"},
		{"GET", "http://devvm.proxy/nix/nar/../" + h32 + ".narinfo"},
		{"GET", "http://devvm.proxy/nix/" + h32 + ".ls"},
		{"GET", "http://devvm.proxy/nix/log/" + h32},
		{"GET", "http://devvm.proxy/nix/"},
		{"GET", "http://devvm.proxy/nix/nar/x"},
	} {
		if code, _ := nixReq(t, s, c.method, c.target); code != http.StatusBadRequest {
			t.Errorf("%s %s: %d, want 400", c.method, c.target, code)
		}
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("upstream got %q", got)
	}
	// What upstream doesn't have, or answers otherwise, isn't kept: Nix goes direct.
	for _, p := range []string{h32 + ".narinfo", "redirect.narinfo"} {
		if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+p); code != 404 && code != 400 {
			t.Errorf("%s: %d", p, code)
		}
	}
	// A narinfo whose signature doesn't hold, and a NAR that isn't the one the narinfo names.
	f.mu.Lock()
	f.files["/"+fixHash+".narinfo"] = []byte(strings.Replace(string(fixture(t, fixHash+".narinfo")), "NarSize: 8848", "NarSize: 8849", 1))
	f.mu.Unlock()
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 404 {
		t.Errorf("a forged narinfo: %d", code)
	}
	f.mu.Lock()
	f.files["/"+fixHash+".narinfo"] = fixture(t, fixHash+".narinfo")
	nar := fixture(t, fixNar)
	nar[100] ^= 1
	f.files["/"+fixNar] = nar
	f.mu.Unlock()
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 200 {
		t.Fatalf("narinfo: %d", code)
	}
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixNar); code != 404 {
		t.Errorf("a corrupted NAR: %d", code)
	}
	f.mu.Lock()
	f.files["/"+fixNar] = append(fixture(t, fixNar), 0)
	f.mu.Unlock()
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixNar); code != 404 {
		t.Errorf("a NAR longer than its FileSize: %d", code)
	}
	if n, _, _ := NixCacheUsage(dir); n != 1 {
		t.Errorf("the cache holds %d files, want the narinfo alone", n)
	}
	if _, err := os.Stat(filepath.Join(dir, fixNar)); err == nil {
		t.Error("a bad NAR was kept")
	}
}

func TestNixCachePolicy(t *testing.T) {
	t.Parallel()
	s, f, _ := useNixCache(t, "github.com\n")
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 404 {
		t.Errorf("without %s in the policy: %d", nixHost, code)
	}
	s.nix.floor = func(context.Context) error { return fmt.Errorf("%s is blocked", nixHost) }
	s.egress.pol = writePolicy(t, nixHost)
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 404 {
		t.Errorf("below the floor: %d", code)
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("upstream got %q", got)
	}
	s.nix.dir = ""
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/nix-cache-info"); code != 404 {
		t.Errorf("no -nix-cache: %d", code)
	}
}

// The cache's own fetch from cache.nixos.org meets the rules by method and path too: a deny of
// its NARs leaves narinfos alone and refuses the NARs, which nothing fetches.
func TestNixCacheRulesByPath(t *testing.T) {
	t.Parallel()
	s, f, _ := useNixCache(t, nixHost+"\n!"+nixHost+" /nar/\n")
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 200 {
		t.Fatalf("a narinfo: %d", code)
	}
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixNar); code != 404 {
		t.Errorf("a NAR a deny names: %d", code)
	}
	if got := f.requests(); len(got) != 1 {
		t.Errorf("upstream got %q, want the narinfo alone", got)
	}
}

// A NAR is fetched once however many ask for it at once, a HEAD fetches nothing, a copy that
// isn't the size its narinfo says goes, and files being fetched never take the cache past its cap.
func TestNixCacheFetches(t *testing.T) {
	t.Parallel()
	s, f, dir := useNixCache(t, nixHost)
	nar := "http://devvm.proxy/nix/" + fixNar
	if code, _ := nixReq(t, s, "GET", "http://devvm.proxy/nix/"+fixHash+".narinfo"); code != 200 {
		t.Fatalf("narinfo: %d", code)
	}
	if code, _ := nixReq(t, s, "HEAD", nar); code != 404 || len(f.requests()) != 1 {
		t.Errorf("HEAD of a NAR not in the cache: %d, upstream got %q", code, f.requests())
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if code, body := nixReq(t, s, "GET", nar); code != 200 || len(body) != 958 {
				t.Errorf("GET: %d, %d bytes", code, len(body))
			}
		})
	}
	wg.Wait()
	if got := f.requests(); len(got) != 2 {
		t.Errorf("upstream got %q, want the narinfo and the NAR once", got)
	}
	if code, _ := nixReq(t, s, "HEAD", nar); code != 200 {
		t.Errorf("HEAD of a NAR in the cache: %d", code)
	}
	// A fetch that ended between this request's look at the cache and its own turn to fetch
	// leaves the NAR there, which it serves without fetching it again.
	must(t, os.Remove(filepath.Join(dir, fixNar)))
	s.nix.floor = func(context.Context) error {
		return os.WriteFile(filepath.Join(dir, fixNar), fixture(t, fixNar), 0o600)
	}
	if code, body := nixReq(t, s, "GET", nar); code != 200 || len(body) != 958 || len(f.requests()) != 2 {
		t.Errorf("a NAR another fetch just kept: %d, %d bytes, upstream got %q", code, len(body), f.requests())
	}
	s.nix.floor = func(context.Context) error { return nil }
	// A copy of another size is dropped and fetched again.
	must(t, os.WriteFile(filepath.Join(dir, fixNar), []byte("short"), 0o600))
	if code, body := nixReq(t, s, "GET", nar); code != 200 || len(body) != 958 || len(f.requests()) != 3 {
		t.Errorf("a short copy: %d, %d bytes, upstream got %q", code, len(body), f.requests())
	}
	// Files being fetched that fill the cap leave no room, and nothing is kept.
	must(t, os.Remove(filepath.Join(dir, fixNar)))
	s.nix.max = 128 << 10
	writeCached(t, dir, ".tmp-busy", 128<<10, time.Now())
	must(t, os.Remove(sizeFile(dir)))
	if code, _ := nixReq(t, s, "GET", nar); code != 404 {
		t.Errorf("a full cache: %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, fixNar)); err == nil {
		t.Error("a NAR was kept in a full cache")
	}
	if n, _ := readSize(dir); n > s.nix.max {
		t.Errorf("the running total is %d, over the cap", n)
	}
}

// writeCached writes a file of size bytes into the cache, last used at.
func writeCached(t *testing.T, dir, name string, size int, at time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.WriteFile(p, make([]byte, size), 0o600))
	must(t, os.Chtimes(p, at, at))
}

func TestTrimNixCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "nar"), 0o700))
	now := time.Now()
	const u = 64 << 10 // a whole number of blocks on any file system
	writeCached(t, dir, "a.narinfo", u, now.Add(-3*time.Hour))
	writeCached(t, dir, "nar/b.nar", u, now.Add(-time.Hour))
	writeCached(t, dir, "c.narinfo", u, now.Add(-2*time.Hour))
	writeCached(t, dir, ".tmp-old", u, now.Add(-2*nixTmpAge))
	writeCached(t, dir, ".tmp-new", u, now)
	if n, size, err := NixCacheUsage(dir); err != nil || n != 3 || size != 3*u {
		t.Fatalf("usage: %d %d %v", n, size, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tmp-old")); err == nil {
		t.Error("a dead proxy's file is still there")
	}
	// The file being written counts against the cap, but stays.
	n, freed, err := TrimNixCache(dir, 2*u+u/2)
	if err != nil || n != 2 || freed != 2*u {
		t.Fatalf("trim: %d %d %v", n, freed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nar/b.nar")); err != nil {
		t.Error("the most recently used file was deleted")
	}
	if n, freed, _ := TrimNixCache(dir, 0); n != 1 || freed != u {
		t.Errorf("emptied: %d %d", n, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tmp-new")); err != nil {
		t.Error("a file being written was deleted")
	}
	// A small file counts the disk it takes.
	writeCached(t, dir, "d.narinfo", 1, now)
	if _, size, _ := NixCacheUsage(dir); size < 512 {
		t.Errorf("a 1-byte file counts %d", size)
	}
	if n, _, err := TrimNixCache(filepath.Join(dir, "none"), 0); n != 0 || err != nil {
		t.Errorf("no cache: %d %v", n, err)
	}
}

// The cache's folders are made the user's alone, and a link planted in nar/'s place, to a folder
// outside the cache or (relative) in it, or a file there, is deleted as itself, never followed, and
// a folder made in its place: what the link led to keeps its mode and its files.
func TestMakeNixFoldersPlanted(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nix-cache")
	must(t, os.Mkdir(dir, 0o750))
	must(t, os.Mkdir(filepath.Join(dir, "nar"), 0o750))
	writeCached(t, dir, "nar/x.nar", 1, time.Now())
	if replaced, err := makeNixFolders(dir); replaced || err != nil {
		t.Fatalf("a cache of its own: replaced %v, %v", replaced, err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "nar")} {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v", d, fi.Mode(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "nar/x.nar")); err != nil {
		t.Errorf("a cached file went: %v", err)
	}
	for _, kind := range []string{"outside", "inside", "a file"} {
		planted := filepath.Join(t.TempDir(), "nix-cache")
		must(t, os.Mkdir(planted, 0o700))
		target := t.TempDir()
		switch kind {
		case "inside":
			target = filepath.Join(planted, "other")
			must(t, os.Mkdir(target, 0o750))
			must(t, os.Symlink("other", filepath.Join(planted, "nar")))
		case "a file":
			must(t, os.WriteFile(filepath.Join(planted, "nar"), nil, 0o600))
		default:
			must(t, os.Symlink(target, filepath.Join(planted, "nar")))
		}
		must(t, os.Chmod(target, 0o750)) //nolint:gosec // a folder of the test's, whose mode must stay
		must(t, os.WriteFile(filepath.Join(target, "mine"), []byte("mine"), 0o600))
		if replaced, err := makeNixFolders(planted); !replaced || err != nil {
			t.Errorf("%s: replaced %v, %v", kind, replaced, err)
		}
		if fi, err := os.Lstat(filepath.Join(planted, "nar")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: nar/ made in its place: %v, %v", kind, fi, err)
		}
		if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o750 {
			t.Errorf("%s: the planted link's target: %v, %v", kind, fi.Mode(), err)
		}
		if b, err := os.ReadFile(filepath.Join(target, "mine")); err != nil || string(b) != "mine" { //nolint:gosec // the test's own file
			t.Errorf("%s: the target's file: %q, %v", kind, b, err)
		}
	}
}

// What a VM's proxy's child could plant in the shared cache leads its trim nowhere: a link in a
// file's place, or in nar/'s, is deleted, or left, as itself, never what it leads to; a hard link
// of another file goes as a name alone; a FIFO is never read or waited on; and a lock file that
// isn't a regular file is refused.
func TestTrimNixCachePlanted(t *testing.T) {
	t.Parallel()
	dir, mine := t.TempDir(), t.TempDir()
	keep := filepath.Join(mine, "keep")
	must(t, os.WriteFile(keep, []byte("mine"), 0o600))
	must(t, os.Mkdir(filepath.Join(mine, "nar"), 0o700))
	writeCached(t, mine, "nar/x.nar", 64<<10, time.Now().Add(-time.Hour))
	must(t, os.Symlink(keep, filepath.Join(dir, "a.narinfo")))
	must(t, os.Symlink(filepath.Join(mine, "nar"), filepath.Join(dir, "nar")))
	must(t, os.Link(keep, filepath.Join(dir, "b.narinfo")))
	must(t, syscall.Mkfifo(filepath.Join(dir, "c.narinfo"), 0o600))
	if n, _, err := NixCacheUsage(dir); err != nil || n != 1 {
		t.Errorf("usage: %d, %v (the hard link's name alone)", n, err)
	}
	if _, _, err := TrimNixCache(dir, 0); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" { //nolint:gosec // the test's own file
		t.Errorf("the planted links' target: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(mine, "nar", "x.nar")); err != nil {
		t.Errorf("a file through the planted nar/ went: %v", err)
	}
	// A lock file that's a FIFO: refused, never waited on.
	other := t.TempDir()
	must(t, syscall.Mkfifo(filepath.Join(other, ".lock"), 0o600))
	if _, _, err := TrimNixCache(other, 0); !errors.Is(err, safefile.ErrNotRegular) {
		t.Errorf("a FIFO for a lock: %v", err)
	}
}

// A lock another process holds is waited for no longer than its wait: ErrNixLocked, which devvm
// gc reports as a skipped trim.
func TestNixLockWaits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock, err := lockNix(dir)
	must(t, err)
	defer unlock()
	start := time.Now()
	if _, err := lockNixWithin(dir, 200*time.Millisecond); !errors.Is(err, ErrNixLocked) {
		t.Errorf("a held lock: %v", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 2*time.Second {
		t.Errorf("waited %v", took)
	}
}

// Many proxies keeping files at once never leave the cache over its cap.
func TestNixCacheCap(t *testing.T) {
	t.Parallel()
	s, _, dir := useNixCache(t, nixHost)
	s.nix.max = 64 << 10
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := range 40 {
		wg.Go(func() {
			if s.nix.keepBytes(filepath.Join(dir, fmt.Sprintf("%032d.narinfo", i)), make([]byte, 900)) != nil {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if failed.Load() > 0 {
		t.Fatalf("%d writes failed", failed.Load())
	}
	if _, size, _ := NixCacheUsage(dir); size > s.nix.max || size == 0 {
		t.Errorf("the cache takes %d, its cap is %d", size, s.nix.max)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("left %s", e.Name())
		}
	}
}

// FuzzNixPath: whatever nixPath takes is one of the exact forms, and names a file inside the cache's
// folder.
func FuzzNixPath(f *testing.F) {
	for _, s := range []string{"nix-cache-info", fixHash + ".narinfo", fixNar, "nar/../x", "../" + fixHash + ".narinfo", fixNar + "/"} {
		f.Add(s)
	}
	exact := regexp.MustCompile(`^(nix-cache-info|[0-9a-df-np-sv-z]{32}\.narinfo|nar/[0-9a-df-np-sv-z]{52}\.nar(\.xz|\.zst|\.bz2)?)$`)
	f.Fuzz(func(t *testing.T, p string) {
		if nixPath(p) == nixNone {
			return
		}
		if !exact.MatchString(p) {
			t.Fatalf("nixPath took %q", p)
		}
		if rel, err := filepath.Rel("/cache", filepath.Join("/cache", p)); err != nil || !filepath.IsLocal(rel) || rel != filepath.FromSlash(p) {
			t.Fatalf("%q leaves the cache's folder", p)
		}
	})
}

// FuzzParseNarinfo: a narinfo the proxy takes, from the cache or anything between, is one
// cache.nixos.org-1 signed, of the path asked for: what the signature covers (its store path,
// NarHash, NarSize and references) is the fixture's, the only signed one the fuzzer has, and what it
// doesn't is held to its forms: a NAR's URL named by its NarHash or FileHash, and sizes from 1 byte
// to 1 TiB.
func FuzzParseNarinfo(f *testing.F) {
	good, err := os.ReadFile(filepath.Join("testdata", "nix", fixHash+".narinfo"))
	if err != nil {
		f.Fatal(err)
	}
	want, err := parseNarinfo(good, fixHash)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good, fixHash)
	f.Add(good, "0p7ahb9j02av0zx4hqmwqdpbdas9i25q")
	f.Add(append(bytes.Clone(good), "FileSize: 1\n"...), fixHash)
	f.Add([]byte(strings.Replace(string(good), "URL: nar/0p7", "URL: nar/1jrm0rjd3i5vxv2sjm6zgqqi87ikb3fjqg3myg7wj0x4q5psb550.nar.zst\nX: ", 1)), fixHash)
	f.Add([]byte(strings.Replace(string(good), "References: ", "References: 00000000000000000000000000000000-evil ", 1)), fixHash)
	f.Fuzz(func(t *testing.T, data []byte, hash string) {
		ni, err := parseNarinfo(data, hash)
		if err != nil {
			return
		}
		refs := slices.Sorted(slices.Values(ni.refs))
		switch {
		case hash != fixHash || ni.storePath != want.storePath || ni.narHash != want.narHash || ni.narSize != want.narSize ||
			!slices.Equal(refs, slices.Sorted(slices.Values(want.refs))):
			t.Fatalf("took a narinfo no one signed, for %q: %+v", hash, ni)
		case nixPath(ni.url) != nixNar || ni.url[4:56] != ni.narHash[7:] && ni.url[4:56] != ni.fileHash[7:]:
			t.Fatalf("took a NAR's URL %q", ni.url)
		case ni.fileSize < 1 || ni.fileSize > 1<<40 || ni.narSize < 1 || ni.narSize > 1<<40:
			t.Fatalf("took sizes %d and %d", ni.fileSize, ni.narSize)
		}
	})
}
