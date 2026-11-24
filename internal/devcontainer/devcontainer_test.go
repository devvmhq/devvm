package devcontainer

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/shell"
)

const ws = "/Users/me/web app"

// shellWord is what a variable's value may be in the VM's environment
// file: single-quoted text, escaped quotes, and the VM's own variables,
// double-quoted. Nothing else a shell would run or split.
var shellWord = regexp.MustCompile(`^(?:'[^']*'|\\'|"\$\{[A-Za-z_][A-Za-z0-9_]*\}")+$`)

// Whatever the file holds, Parse doesn't panic, refuses ${localEnv:…}
// and ${env:…}, and its variables are names and shell words the
// environment file can hold as they are; nothing it keeps has a NUL.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`{"containerEnv": {"A": "x'y ${containerEnv:PATH} $(id) ${HOME}\n"}, "remoteEnv": {"B": null}}`,
		`{"postCreateCommand": {"a": ["x", "${containerWorkspaceFolder}"], "b": "echo ${containerEnv:A}"}}`,
		`{"forwardPorts": [1, "localhost:2", "127.0.0.1:65536", "0.0.0.0:3"], "image": "a/b:c"}`,
		"// c\n{\"a\": /* x */ 1,}", `{"remoteEnv": {"A": "${localEnv:HOME}"}}`, `{"x": "\u0000"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(File, data, ws)
		if err != nil {
			return
		}
		for _, v := range p.Env {
			if !shell.EnvName(v.Name) || !shellWord.MatchString(v.Value) || hostVar(v.Value) {
				t.Errorf("variable %q=%q", v.Name, v.Value)
			}
		}
		kept := []string{p.Image}
		for _, c := range slices.Concat(p.Create, p.Start) {
			kept = append(kept, c.Argv...)
		}
		for _, s := range kept {
			if strings.IndexByte(s, 0) >= 0 || hostVar(s) {
				t.Errorf("kept %q", s)
			}
		}
	})
}

// hostVar reports whether s holds a variable of this machine's
// environment that Parse should have refused. It reads variables as
// subst does, each ${ to the first } after it, so ${${localEnv:} is one
// variable whose name starts with ${, left as text.
func hostVar(s string) bool {
	for _, v := range variable.FindAllString(s, -1) {
		if name := v[2 : len(v)-1]; strings.HasPrefix(name, "localEnv:") || strings.HasPrefix(name, "env:") {
			return true
		}
	}
	return false
}

// variable is a ${…} as subst reads it, to the first } after its ${.
var variable = regexp.MustCompile(`\$\{[^}]*\}`)

func parse(t *testing.T, src string) (Plan, error) {
	t.Helper()
	return Parse(File, []byte(src), ws)
}

func names(ms []msg.M) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Names())
	}
	return out
}

func TestParse(t *testing.T) {
	p, err := parse(t, `{
	// The project's container.
	"name": "web",
	"$schema": "x",
	"customizations": {"vscode": {}},
	"image": "mcr.microsoft.com/devcontainers/base:ubuntu", /* its base */
	"features": {
		"ghcr.io/devcontainers/features/docker-in-docker:2": {},
		"ghcr.io/devcontainers/features/node:1": {"version": "22"},
		"ghcr.io/devcontainers/features/go@sha256:abc": "",
	},
	"forwardPorts": [3000, "localhost:5173", "127.0.0.1:8000", "db:5432", 3000, 0],
	"containerEnv": {"A": "it's ${containerWorkspaceFolder}", "B": "x"},
	"remoteEnv": {"B": "y", "PATH": "${containerEnv:PATH}:/opt/x", "C": null, "E": "", "N": "${${localEnv:HOME}}"},
	"onCreateCommand": "echo ${containerWorkspaceFolderBasename} $HOME ${HOME}",
	"updateContentCommand": ["npm", "ci"],
	"postCreateCommand": {"b": "echo b", "a": ["echo", "a"], "empty": ""},
	"postStartCommand": "echo $PATH ${containerEnv:PATH} ${${localEnv:HOME}}",
	"postAttachCommand": "x",
	"initializeCommand": "rm -rf ~",
	"runArgs": ["--privileged"],
	"mounts": [],
	"bogus": 1,
}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.File != File || len(p.Sum) != 64 {
		t.Errorf("file %q sum %q", p.File, p.Sum)
	}
	if p.Image != "mcr.microsoft.com/devcontainers/base:ubuntu" {
		t.Errorf("image = %q", p.Image)
	}
	if want := []string{"docker", "go", "node"}; !slices.Equal(p.Layers, want) {
		t.Errorf("layers = %q, want %q", p.Layers, want)
	}
	if p.Ports != "3000,5173,8000" {
		t.Errorf("ports = %q", p.Ports)
	}
	wantEnv := []Var{
		{"A", `'it'\''s ` + ws + `'`},
		{"B", "'y'"},
		{"E", "''"},
		{"N", `'${${localEnv:HOME}}'`}, // one variable named ${localEnv:HOME, left as text
		{"PATH", `"${PATH}"':/opt/x'`},
	}
	if !slices.Equal(p.Env, wantEnv) {
		t.Errorf("env = %q, want %q", p.Env, wantEnv)
	}
	wantCreate := []Command{
		{OnCreate, []string{"sh", "-c", "echo web app $HOME ${HOME}"}},
		{UpdateContent, []string{"npm", "ci"}},
		{PostCreate + " a", []string{"echo", "a"}},
		{PostCreate + " b", []string{"sh", "-c", "echo b"}},
	}
	if !slices.EqualFunc(p.Create, wantCreate, cmdEqual) {
		t.Errorf("create = %q, want %q", p.Create, wantCreate)
	}
	wantStart := []Command{{PostStart, []string{"sh", "-c", "echo $PATH ${PATH} ${${localEnv:HOME}}"}}}
	if !slices.EqualFunc(p.Start, wantStart, cmdEqual) {
		t.Errorf("start = %q, want %q", p.Start, wantStart)
	}
	wantNotes := []string{
		"devcontainer_initialize file=" + File,
		"devcontainer_feature_options features=ghcr.io/devcontainers/features/node:1 file=" + File,
		`devcontainer_ports_ignored file=` + File + ` ports="db:5432" 0`,
		"devcontainer_ignored file=" + File + " keys=mounts postAttachCommand runArgs",
		"devcontainer_unknown file=" + File + " keys=bogus",
	}
	if got := names(p.Notes); !slices.Equal(got, wantNotes) {
		t.Errorf("notes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantNotes, "\n"))
	}
}

func cmdEqual(a, b Command) bool { return a.Name == b.Name && slices.Equal(a.Argv, b.Argv) }

func TestParseEmpty(t *testing.T) {
	p, err := parse(t, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != "" || p.Layers != nil || p.Ports != "" || p.Env != nil || p.Create != nil || p.Start != nil || p.Notes != nil {
		t.Errorf("plan = %+v", p)
	}
}

// The plan's parts go into a VM's state as JSON, one line.
func TestPlanJSON(t *testing.T) {
	p, err := parse(t, `{"remoteEnv": {"A": "x\ny"}, "postStartCommand": "a\nb"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{p.Env, p.Start} {
		b, err := json.Marshal(v)
		if err != nil || strings.ContainsAny(string(b), "\n\r") {
			t.Errorf("%s, %v", b, err)
		}
	}
}

func TestParseRefused(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"syntax", "{\n  \"a\": 1\n  \"b\": 2\n}", "devcontainer_invalid column=3 file=" + File + " line=3"},
		{"syntax after a comment", "// c\n{\"a\": }", "devcontainer_invalid column=7 file=" + File + " line=2"},
		{"truncated", `{"a": 1`, "devcontainer_invalid column=7 file=" + File + " line=1"},
		{"not an object", "\n  [1]", "devcontainer_invalid column=3 file=" + File + " line=2"},
		{"null", "null", "devcontainer_invalid column=1 file=" + File + " line=1"},
		{"empty", "", "devcontainer_invalid column=1 file=" + File + " line=1"},
		{"image kind", `{"image": 1}`, "devcontainer_bad_value file=" + File + " key=image"},
		{"ports kind", `{"forwardPorts": [true]}`, "devcontainer_bad_value file=" + File + " key=forwardPorts"},
		{"env kind", `{"containerEnv": {"A": 1}}`, "devcontainer_bad_value file=" + File + " key=containerEnv"},
		{"container env null", `{"containerEnv": {"A": null}}`, "devcontainer_bad_value file=" + File + " key=containerEnv"},
		{"command kind", `{"postCreateCommand": 1}`, "devcontainer_bad_value file=" + File + " key=postCreateCommand"},
		{"named command kind", `{"postStartCommand": {"a": 1}}`, "devcontainer_bad_value file=" + File + " key=postStartCommand"},
		{"nul", `{"postCreateCommand": "a\u0000b"}`, "devcontainer_bad_value file=" + File + " key=postCreateCommand"},
		{"env name", `{"remoteEnv": {"A-B": "x"}}`, "devcontainer_env_name file=" + File + " name=A-B"},
		{"build", `{"build": {"dockerfile": "Dockerfile"}}`, "devcontainer_build file=" + File + " key=build"},
		{"dockerFile", `{"dockerFile": "Dockerfile"}`, "devcontainer_build file=" + File + " key=dockerFile"},
		{"compose", `{"dockerComposeFile": "c.yml", "service": "x"}`, "devcontainer_build file=" + File + " key=dockerComposeFile"},
		{"localEnv in env", `{"containerEnv": {"T": "${localEnv:GITHUB_TOKEN}"}}`, "devcontainer_local_env file=" + File + " variable=${localEnv:GITHUB_TOKEN}"},
		{"localEnv default", `{"remoteEnv": {"T": "${localEnv:HOME:/x}"}}`, "devcontainer_local_env file=" + File + " variable=${localEnv:HOME:/x}"},
		{"env alias", `{"postCreateCommand": "echo ${env:HOME}"}`, "devcontainer_local_env file=" + File + " variable=${env:HOME}"},
		{"localEnv in image", `{"image": "${localEnv:IMG}"}`, "devcontainer_local_env file=" + File + " variable=${localEnv:IMG}"},
		{"localEnv in an argument", `{"postStartCommand": ["echo", "${localEnv:X}"]}`, "devcontainer_local_env file=" + File + " variable=${localEnv:X}"},
		{"localEnv in a port", `{"forwardPorts": ["${localEnv:P}"]}`, "devcontainer_local_env file=" + File + " variable=${localEnv:P}"},
		{"local folder", `{"postCreateCommand": "ls ${localWorkspaceFolder}"}`, "devcontainer_variable file=" + File + " variable=${localWorkspaceFolder}"},
		{"local basename", `{"remoteEnv": {"A": "${localWorkspaceFolderBasename}"}}`, "devcontainer_variable file=" + File + " variable=${localWorkspaceFolderBasename}"},
		{"devcontainerId", `{"remoteEnv": {"A": "${devcontainerId}"}}`, "devcontainer_variable file=" + File + " variable=${devcontainerId}"},
		{"containerEnv in an argument", `{"postCreateCommand": ["echo", "${containerEnv:PATH}"]}`, "devcontainer_variable file=" + File + " variable=${containerEnv:PATH}"},
		{"containerEnv with a default", `{"remoteEnv": {"A": "${containerEnv:PATH:/bin}"}}`, "devcontainer_variable file=" + File + " variable=${containerEnv:PATH:/bin}"},
		{"feature", `{"features": {"ghcr.io/devcontainers/features/java:1": {}}}`, "devcontainer_feature feature=ghcr.io/devcontainers/features/java:1 file=" + File},
		{"another publisher's node", `{"features": {"ghcr.io/someone/features/node:1": {}}}`, "devcontainer_feature feature=ghcr.io/someone/features/node:1 file=" + File},
		{"docker outside", `{"features": {"ghcr.io/devcontainers/features/docker-outside-of-docker:1": {}}}`, "devcontainer_docker_outside feature=ghcr.io/devcontainers/features/docker-outside-of-docker:1 file=" + File},
		{"features kind", `{"features": ["node"]}`, "devcontainer_bad_value file=" + File + " key=features"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.src)
			if msg.IDOf(err) == "" {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
			if got := msg.NamesOf(err); got != tc.want {
				t.Errorf("error = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fsys     fstest.MapFS
		file     string
		data     string
		wantErr  error
		wantNone bool
	}{
		{name: "none", fsys: fstest.MapFS{"x": {}}, wantNone: true},
		{name: "folder's first", fsys: fstest.MapFS{File: {Data: []byte("a")}, RootFile: {Data: []byte("b")}}, file: File, data: "a"},
		{name: "root", fsys: fstest.MapFS{RootFile: {Data: []byte("b")}}, file: RootFile, data: "b"},
		{name: "a folder", fsys: fstest.MapFS{File + "/x": {}}, file: File, wantErr: ErrNotFile},
		{name: "too large", fsys: fstest.MapFS{File: {Data: make([]byte, maxSize+1)}}, file: File, wantErr: ErrTooLarge},
		{name: "a fifo", fsys: fstest.MapFS{RootFile: {Mode: fs.ModeNamedPipe}}, file: RootFile, wantErr: ErrNotFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, data, err := Find(tc.fsys)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantNone && file != "" || !tc.wantNone && file != tc.file || err == nil && string(data) != tc.data {
				t.Errorf("Find = %q %q", file, data)
			}
		})
	}
}

// A link out of the project folder isn't followed through os.Root's file
// system; one inside it is.
func TestFindLinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"image": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, File)); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, data, err := Find(root.FS()); err == nil {
		t.Errorf("a link out of the folder was read: %q", data)
	}
	if err := os.Remove(filepath.Join(dir, File)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dc.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dc.json", filepath.Join(dir, File)); err != nil {
		t.Fatal(err)
	}
	if file, data, err := Find(root.FS()); err != nil || file != File || string(data) != "{}" {
		t.Errorf("Find = %q %q %v", file, data, err)
	}
}
