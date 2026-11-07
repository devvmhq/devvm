package trust

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Host: the host's bundle and the user's CAs in one, which SSL_CERT_FILE
// names; SSL_CERT_DIR names the host's folders of CAs only when the
// environment has neither variable (TestHostOwnBundle), so a program
// given its own SSL_CERT_FILE trusts no folder it didn't; nothing for no
// CAs.
func TestHost(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "own.pem")
	write(t, dir, "own.pem", []byte("the host's roots"))
	ca := cert(t, "company", true)
	cas, err := parse("company.pem", ca)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "devvm-ca.pem")
	cases := []struct {
		name       string
		env        map[string]string
		cas        CAs
		wantBundle string
		wantEnv    []string // without SSL_CERT_DIR's host folders
		hostDirs   bool     // SSL_CERT_DIR names the host's folders
	}{
		{name: "no CAs", env: map[string]string{"SSL_CERT_FILE": own}},
		{
			name: "SSL_CERT_FILE's bundle", env: map[string]string{"SSL_CERT_FILE": own, "SSL_CERT_DIR": dir}, cas: cas,
			wantBundle: "the host's roots\n" + string(ca), wantEnv: []string{"SSL_CERT_FILE=" + path},
		},
		{
			// Its SSL_CERT_FILE alone: the program read no folder, so it
			// reads none still.
			name: "no SSL_CERT_DIR", env: map[string]string{"SSL_CERT_FILE": own}, cas: cas,
			wantBundle: "the host's roots\n" + string(ca), wantEnv: []string{"SSL_CERT_FILE=" + path},
		},
		{
			name: "SSL_CERT_FILE missing", env: map[string]string{"SSL_CERT_FILE": filepath.Join(dir, "none"), "SSL_CERT_DIR": dir}, cas: cas,
			wantBundle: string(ca), wantEnv: []string{"SSL_CERT_FILE=" + path},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bundle, env, err := c.cas.Host(path, func(k string) string { return c.env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if string(bundle) != c.wantBundle {
				t.Errorf("bundle = %q, want %q", bundle, c.wantBundle)
			}
			var dirs []string
			env = slices.DeleteFunc(env, func(kv string) bool {
				v, ok := strings.CutPrefix(kv, "SSL_CERT_DIR=")
				if ok {
					dirs = strings.Split(v, ":")
				}
				return ok
			})
			if !slices.Equal(env, c.wantEnv) {
				t.Errorf("env = %q, want %q", env, c.wantEnv)
			}
			var want []string
			for _, d := range hostDirs() {
				if fi, err := os.Stat(d); c.hostDirs && err == nil && fi.IsDir() {
					want = append(want, d)
				}
			}
			if !slices.Equal(dirs, want) {
				t.Errorf("SSL_CERT_DIR = %q, want %q", dirs, want)
			}
		})
	}
}

// Without SSL_CERT_FILE the host's bundle is the first of hostBundles
// there, and with neither variable SSL_CERT_DIR names the host's folders
// of CAs, which the program read; a bundle that can't be read fails it.
func TestHostOwnBundle(t *testing.T) {
	cas, err := parse("company.pem", cert(t, "company", true))
	if err != nil {
		t.Fatal(err)
	}
	none := func(string) string { return "" }
	bundle, env, err := cas.Host("/x/devvm-ca.pem", none)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, d := range hostDirs() {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	want := []string{"SSL_CERT_FILE=/x/devvm-ca.pem"}
	if len(dirs) > 0 {
		want = append(want, "SSL_CERT_DIR="+strings.Join(dirs, ":"))
	}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
	if first := firstThere(hostBundles()); first != "" {
		own, err := os.ReadFile(first) //nolint:gosec // the host's bundle
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(bundle), string(own)) {
			t.Errorf("the bundle doesn't start with %s", first)
		}
	}
	if !strings.HasSuffix(string(bundle), string(cas.Bundle())) {
		t.Error("the bundle doesn't end with the CAs")
	}
	dir := t.TempDir()
	if _, _, err := cas.Host("/x/devvm-ca.pem", func(k string) string {
		if k == "SSL_CERT_FILE" {
			return dir
		}
		return ""
	}); err == nil {
		t.Error("a folder for SSL_CERT_FILE read")
	}
	if firstThere([]string{filepath.Join(dir, "none"), dir}) != dir || firstThere(nil) != "" {
		t.Error("firstThere")
	}
}
