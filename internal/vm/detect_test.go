package vm

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// touchAll makes empty files.
func touchAll(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// detect is detectFiles' names alone.
func detect(dir string) []string {
	var names []string
	for _, f := range detectFiles(dir) {
		names = append(names, f.Name)
	}
	return names
}

func TestDetect(t *testing.T) {
	d := t.TempDir()
	if got := detect(d); got != nil {
		t.Errorf("detect(empty) = %v", got)
	}
	for _, tc := range []struct{ file, want string }{
		{"package.json", "node"},
		{"bun.lockb", "node"},
		{"uv.lock", "python"},
		{"requirements.txt", "python"},
		{"Pipfile", "python"},
		{"Cargo.toml", "rust"},
		{"go.mod", "go"},
		{"App.sln", "dotnet"},
		{"App.slnx", "dotnet"},
		{"App.csproj", "dotnet"},
		{"Lib.fsproj", "dotnet"},
		{"global.json", "dotnet"},
		{"flake.nix", "nix"},
		{"shell.nix", "nix"},
		{"devenv.nix", "nix"},
	} {
		p := filepath.Join(d, tc.file)
		touchAll(t, p)
		if got := detect(d); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("detect with %s = %v, want %s", tc.file, got, tc.want)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	// Several, in a fixed order; only the top level counts.
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	touchAll(t, filepath.Join(d, "go.mod"), filepath.Join(d, "package.json"), filepath.Join(d, "pyproject.toml"),
		filepath.Join(d, "sub", "Cargo.toml"))
	if got := detect(d); !slices.Equal(got, []string{"node", "python", "go"}) {
		t.Errorf("detect = %v", got)
	}
	touchAll(t, filepath.Join(d, "flake.nix"))
	if got := detect(d); !slices.Equal(got, []string{"node", "python", "go", "nix"}) {
		t.Errorf("detect with flake.nix = %v", got)
	}
	// Each with the first of its files that calls for it.
	want := []detected{{"node", "package.json"}, {"python", "pyproject.toml"}, {"go", "go.mod"}, {"nix", "flake.nix"}}
	if got := detectFiles(d); !slices.Equal(got, want) {
		t.Errorf("detectFiles = %v, want %v", got, want)
	}
}

// A pattern names the file it matched: the first, in name order, of the
// first pattern that matches, before the names after it; only the top
// level counts, and a name that only looks like it doesn't.
func TestDetectPatterns(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	touchAll(t, filepath.Join(d, "src", "App.csproj"), filepath.Join(d, "App.csproj.user"), filepath.Join(d, "global.json.bak"))
	if got := detect(d); got != nil {
		t.Errorf("detect with src/App.csproj, App.csproj.user and global.json.bak = %v", got)
	}
	touchAll(t, filepath.Join(d, "global.json"), filepath.Join(d, "Web.csproj"), filepath.Join(d, "Api.csproj"))
	if got, want := detectFiles(d), []detected{{"dotnet", "Api.csproj"}}; !slices.Equal(got, want) {
		t.Errorf("detectFiles = %v, want %v", got, want)
	}
	touchAll(t, filepath.Join(d, "Shop.sln"))
	if got, want := detectFiles(d), []detected{{"dotnet", "Shop.sln"}}; !slices.Equal(got, want) {
		t.Errorf("detectFiles with Shop.sln = %v, want %v", got, want)
	}
	if got := detectFiles(filepath.Join(d, "none")); got != nil {
		t.Errorf("detectFiles of a missing folder = %v", got)
	}
}

// A default.nix calls for nix only when it makes a shell, and only as a
// regular file.
func TestDetectDefaultNix(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "default.nix")
	for _, tc := range []struct {
		body string
		want []string
	}{
		{"{ pkgs ? import <nixpkgs> {} }: pkgs.mkShell { packages = [ pkgs.hello ]; }", []string{"nix"}},
		{"{ pkgs ? import <nixpkgs> {} }: pkgs.mkShellNoCC { }", []string{"nix"}},
		{"{ pkgs ? import <nixpkgs> {} }: pkgs.callPackage ./package.nix { }", nil},
	} {
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := detect(d); !slices.Equal(got, tc.want) {
			t.Errorf("detect with default.nix %q = %v, want %v", tc.body, got, tc.want)
		}
	}
	shell := filepath.Join(t.TempDir(), "shell.nix")
	if err := os.WriteFile(shell, []byte("pkgs.mkShell { }"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shell, p); err != nil {
		t.Fatal(err)
	}
	if got := detect(d); got != nil {
		t.Errorf("detect with a linked default.nix = %v", got)
	}
}
