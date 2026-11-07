package tools

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
)

// testPassword is the company proxy's password the tests' lookups give;
// it is never in a process's arguments.
const testPassword = "pa55 word"

// saved is a lookup of the company proxy's password that has it.
func saved(context.Context, string) (string, error) { return testPassword, nil }

// The host's mise is downloaded through config.toml's company proxy, with
// the user's password from devvm's secrets, when the environment sets
// none; without the password saved, it says so, and downloads nothing.
func TestHostMiseThroughCompanyProxy(t *testing.T) {
	body := []byte("#!/bin/sh\necho mise\n")
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.RequestURI+" "+r.Header.Get("Proxy-Authorization"))
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	sum, err := image.SHA256File(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.Mise = ""
	e.OS, e.Arch = "linux", "amd64"
	e.miseURL = "http://mise.example/v%[1]s/%[2]s"
	e.miseSums = map[string]string{"linux-x64": sum}
	write(t, e.Dirs.ConfigFile(), []byte("[proxy]\nhttp = \"http://alice@"+host+"\"\n"))
	if _, err := e.HostMise(t.Context()); msg.NamesOf(err) != "proxy_password_missing" || got != nil {
		t.Errorf("no password saved: %v, %q", msg.NamesOf(err), got)
	}
	e = newEnv(t)
	e.Mise = ""
	e.OS, e.Arch = "linux", "amd64"
	e.miseURL = "http://mise.example/v%[1]s/%[2]s"
	e.miseSums = map[string]string{"linux-x64": sum}
	write(t, e.Dirs.ConfigFile(), []byte("[proxy]\nhttp = \"http://alice@"+host+"\"\n"))
	e.Proxy.Password = saved
	if _, err := e.HostMise(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:"+testPassword))
	if want := "GET http://mise.example/v" + miseVersion + "/linux-x64 " + auth; !slices.Equal(got, []string{want}) {
		t.Errorf("the company proxy got %q, want %q", got, want)
	}
}

// mise runs with config.toml's company proxy in its environment, never
// its arguments; with the environment's, it gets nothing more.
func TestMiseCmdCompanyProxy(t *testing.T) {
	e := newEnv(t)
	write(t, e.Dirs.ConfigFile(), []byte("[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\nno-proxy = \".corp\"\n"))
	e.Proxy.Password = saved
	cmd, err := e.miseCmd(t.Context(), "/bin/true", t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	withPassword := "HTTPS_PROXY=http://" + url.UserPassword("alice", testPassword).String() + "@proxy.corp:3128"
	if !slices.Contains(cmd.Env, withPassword) || !slices.Contains(cmd.Env, "NO_PROXY=.corp") {
		t.Errorf("env %q", cmd.Env)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "pa55") {
		t.Errorf("args %q", cmd.Args)
	}
	// The environment's: its own, inherited, and config.toml's not added.
	e.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "http://env.corp:8080"}[k] }
	cmd, err = e.miseCmd(t.Context(), "/bin/true", t.TempDir(), "lock")
	if err != nil || slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.Contains(kv, "proxy.corp") }) {
		t.Errorf("the environment's: %v, %q", err, cmd.Env)
	}
	// None: nothing added to mise's own eight.
	e.Proxy = nil
	if cmd, err = e.miseCmd(t.Context(), "/bin/true", t.TempDir(), "lock"); err != nil || len(cmd.Env) != len(os.Environ())+8 {
		t.Errorf("none: %v, %d of %d", err, len(cmd.Env), len(os.Environ()))
	}
}

// A tool build checks the company proxy before anything starts.
func TestBuildChecksCompanyProxy(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	write(t, filepath.Join(e.Dirs.Config, "config.toml"), []byte("[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\n"))
	f := fake()
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, e.files(t, "node")); msg.NamesOf(err) != "proxy_password_missing" {
		t.Errorf("Build = %v", msg.NamesOf(err))
	}
	if len(f.Calls()) > 0 {
		t.Errorf("calls = %q", f.Methods())
	}
}
