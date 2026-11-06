package tools

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

func TestDotnetRID(t *testing.T) {
	for platform, want := range map[string]string{
		"linux-arm64": "linux-arm64", "linux-x64": "linux-x64",
		"linux-arm64-musl": "linux-musl-arm64", "linux-x64-musl": "linux-musl-x64",
	} {
		if got := dotnetRID(platform); got != want {
			t.Errorf("dotnetRID(%s) = %s, want %s", platform, got, want)
		}
	}
}

// sha512Of is a made-up SHA-512 in hex, one per letter.
func sha512Of(c string) string { return strings.Repeat(c, 128) }

// dotnetChannel is a channel's releases.json as Microsoft writes it, with
// 10.0.402 as a release's SDK and 10.0.110 among its others; 10.0.402's
// files: good ones for arm64 and musl arm64 (an uppercase hash), and for
// x64 one off Microsoft's downloads and one without a SHA-512.
const dotnetChannel = `{"channel-version": "10.0", "releases": [
  {"release-version": "10.0.4", "sdk": {"version": "10.0.402", "files": [
    {"name": "dotnet-sdk-linux-arm64.tar.gz", "rid": "linux-arm64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-arm64.tar.gz", "hash": "` + "aaaa" + `"},
    {"name": "dotnet-sdk-linux-arm64.zip", "rid": "linux-arm64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-arm64.zip", "hash": "HASH_B"},
    {"name": "dotnet-sdk-linux-arm64.tar.gz", "rid": "linux-arm64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-arm64.tar.gz", "hash": "HASH_A"},
    {"name": "dotnet-sdk-linux-musl-arm64.tar.gz", "rid": "linux-musl-arm64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-musl-arm64.tar.gz", "hash": "HASH_C_UPPER"},
    {"name": "dotnet-sdk-linux-x64.tar.gz", "rid": "linux-x64", "url": "https://example.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-x64.tar.gz", "hash": "HASH_D"},
    {"name": "dotnet-sdk-linux-musl-x64.tar.gz", "rid": "linux-musl-x64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-musl-x64.tar.gz", "hash": "abc"}
  ]}, "sdks": [{"version": "10.0.402", "files": []}, {"version": "10.0.110", "files": [
    {"name": "dotnet-sdk-linux-arm64.tar.gz", "rid": "linux-arm64", "url": "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.110/dotnet-sdk-10.0.110-linux-arm64.tar.gz", "hash": "HASH_E"}
  ]}]},
  {"release-version": "10.0.0-rc.1", "sdk": null}
]}`

// channelJSON is dotnetChannel with its made-up hashes.
func channelJSON() string {
	return strings.NewReplacer("HASH_A", sha512Of("a"), "HASH_B", sha512Of("b"), "HASH_C_UPPER", strings.ToUpper(sha512Of("c")),
		"HASH_D", sha512Of("d"), "HASH_E", sha512Of("e")).Replace(dotnetChannel)
}

// dotnetServer serves channel 10.0's releases.json, counting its requests;
// any other channel is missing.
func dotnetServer(t *testing.T, e env) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/10.0/releases.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(channelJSON()))
	}))
	t.Cleanup(srv.Close)
	e.dotnetURL = srv.URL + "/%s/releases.json"
	return &n
}

// dotnetLock is the fixture with dotnet's entries as mise locks them,
// versions without downloads.
func dotnetLock(t *testing.T, versions ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	in := false
	for _, line := range lines(fixture(t)) {
		if strings.HasPrefix(line, "[") {
			in = strings.HasPrefix(line, "[[tools.dotnet]]") || strings.HasPrefix(line, "[tools.dotnet.")
		}
		if !in {
			b.WriteString(line + "\n")
		}
	}
	for _, v := range versions {
		b.WriteString("\n[[tools.dotnet]]\nversion = \"" + v + "\"\nbackend = \"core:dotnet\"\nspecifiers = [\"latest\"]\n")
	}
	return b.Bytes()
}

// Microsoft's archive and its SHA-512 for each of the VMs' platforms join
// each dotnet entry that has none: from its channel's releases.json, read
// once for all of them, a release's SDK or one of its others; a file off
// Microsoft's downloads, without a SHA-512 or not a tar.gz isn't taken. A
// lock that has them all stays as it is, and so does a version Microsoft
// doesn't list.
func TestWithSDKDownloads(t *testing.T) {
	e := newEnv(t)
	n := dotnetServer(t, e)
	lock, err := e.withSDKDownloads(t.Context(), parsed(t, dotnetLock(t, "10.0.402", "10.0.110", "10.0.499", "10.x")))
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
	want := map[string][2]string{
		"10.0.402 linux-arm64":      {"https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-arm64.tar.gz", "sha512:" + sha512Of("a")},
		"10.0.402 linux-arm64-musl": {"https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.402/dotnet-sdk-10.0.402-linux-musl-arm64.tar.gz", "sha512:" + sha512Of("c")},
		"10.0.110 linux-arm64":      {"https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.110/dotnet-sdk-10.0.110-linux-arm64.tar.gz", "sha512:" + sha512Of("e")},
	}
	for _, v := range []string{"10.0.402", "10.0.110", "10.0.499", "10.x"} {
		for _, p := range []string{"linux-arm64", "linux-arm64-musl", "linux-x64", "linux-x64-musl"} {
			table, _ := lock.versionEntry("dotnet", v)["platforms."+p].(map[string]any)
			got := [2]string{str(table, "url"), str(table, "checksum")}
			if got != want[v+" "+p] {
				t.Errorf("%s on %s: %q, want %q", v, p, got, want[v+" "+p])
			}
		}
	}
	if url, sum := lock.Download("dotnet", "linux-arm64"); url != want["10.0.402 linux-arm64"][0] || sum != want["10.0.402 linux-arm64"][1] {
		t.Errorf("Download = %s %s", url, sum)
	}
	if !slices.Equal(lockTools(t, lock.Bytes()), lockTools(t, fixture(t))) || !bytes.Equal(entryOf(t, lock.Bytes(), "node"), entryOf(t, fixture(t), "node")) {
		t.Error("the other tools changed")
	}
	for _, have := range [][]byte{fixture(t), dotnetLock(t)} {
		again, err := e.withSDKDownloads(t.Context(), parsed(t, have))
		if err != nil || !bytes.Equal(again.Bytes(), have) {
			t.Errorf("a lock with every download changed: %v", err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d requests for locks with every download", got)
	}
	// x64's: one off Microsoft's downloads, and one without a SHA-512.
	sdks, err := dotnetSDKs([]byte(channelJSON()))
	if err != nil || len(sdks) != 3 {
		t.Fatalf("dotnetSDKs = %d, %v", len(sdks), err)
	}
	for _, p := range []string{"linux-x64", "linux-x64-musl"} {
		if url, _, ok := sdks[0].download(p); ok {
			t.Errorf("%s: %s", p, url)
		}
	}
}

// A channel Microsoft doesn't answer for fails, and so does Resolve, with
// nothing written; with its downloads, Resolve writes them.
func TestResolveSDKDownloads(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	n := dotnetServer(t, e)
	e.stubLocks(t, dotnetLock(t, "9.0.300"), nil, "")
	if err := e.Refresh(t.Context()); msg.NamesOf(err) != "tools_resolve_failed" {
		t.Errorf("Refresh with a channel that isn't there = %v", msg.NamesOf(err))
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), fixture(t)) {
		t.Error("the lock changed")
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d requests for a 404, want 1", got)
	}
	e.stubLocks(t, dotnetLock(t, "10.0.402"), nil, "")
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	lock := parsed(t, read(t, e.Dirs.ToolLock()))
	if lock.Version("dotnet") != "10.0.402" || !lock.HasDownload("dotnet", e.platforms()) {
		t.Errorf("dotnet = %s, with downloads %v", lock.Version("dotnet"), lock.HasDownload("dotnet", e.platforms()))
	}
	if url, _ := lock.Download("dotnet", "linux-arm64-musl"); !strings.HasSuffix(url, "-linux-musl-arm64.tar.gz") {
		t.Errorf("musl's download = %s", url)
	}
}

// The newest SDK without a download for a platform steps back, to an
// older one whose downloads devvm added too.
func TestSDKStepsBack(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	dotnetServer(t, e)
	e.stubLocks(t, dotnetLock(t, "10.0.499"), dotnetLock(t, "10.0.402"), "10.0.300 10.0.402 10.0.499")
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(*e.said, "devvm: tool_stepped_back older=10.0.402 tool=dotnet version=10.0.499") {
		t.Errorf("said %q", *e.said)
	}
	if lock := parsed(t, read(t, e.Dirs.ToolLock())); lock.Version("dotnet") != "10.0.402" || !lock.HasDownload("dotnet", e.platforms()) {
		t.Errorf("dotnet = %s", lock.Version("dotnet"))
	}
}

// build.sh gets dotnet's download for the VM's platform and its SHA-512,
// in hex, after its tool, version and file; a download without a SHA-512
// isn't built.
func TestBuildDotnet(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	lock := parsed(t, fixture(t))
	for _, tg := range []struct {
		target   Target
		base     string
		platform string
	}{{gnuT, baseKey, "linux-arm64"}, {muslT, muslKey, "linux-arm64-musl"}} {
		f := fake()
		file, err := e.File(lock, "dotnet", tg.target)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Build(t.Context(), backend.Review{}, f, tg.base, tg.target, []string{file}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		url, sum := lock.Download("dotnet", tg.platform)
		spec, _ := f.Calls()[0].Args[0].(backend.ToolsSpec)
		if want := []string{"dotnet", "10.0.401", file, url, strings.TrimPrefix(sum, "sha512:")}; !slices.Equal(spec.Args, want) {
			t.Errorf("%s: build.sh's arguments %q, want %q", tg.platform, spec.Args, want)
		}
	}
	e = newEnv(t)
	sha256 := bytes.ReplaceAll(fixture(t), []byte(`checksum = "sha512:`), []byte(`checksum = "sha256:`))
	e.locked(t, sha256)
	file, err := e.File(parsed(t, sha256), "dotnet", gnuT)
	if err != nil {
		t.Fatal(err)
	}
	f := fake()
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, []string{file}); msg.NamesOf(err) != "tool_unbuilt tool=dotnet version=10.0.401" {
		t.Errorf("a download without a SHA-512 = %v", msg.NamesOf(err))
	}
	if len(f.Calls()) != 0 {
		t.Errorf("built: %q", f.Methods())
	}
	e.noLeftovers(t)
}
