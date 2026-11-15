package project

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/vmname"
)

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"My_App.v2", "my-app-v2"},
		{"My App", "my-app"},
		{"--web--", "web"},
		{"café", "caf"},
		{"___", "root"},
		{"/", "root"},
		{"a-very-long-folder-name-for-a-project", "a-very-long-folder-n"},
		{"a-very-long-folder-----x", "a-very-long-folder"}, // cut, then no trailing dash
	} {
		if got := Slug(tc.base, slugMax); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.base, got, tc.want)
		}
		// vmname's grammar takes every name New makes.
		if f, err := New("/w/"+tc.base, 0); tc.base != "/" && (err != nil || !f.Name.IsProject()) {
			t.Errorf("New(%q) = %q, %v: not a VM's name", tc.base, f.Name, err)
		}
	}
}

func TestNew(t *testing.T) {
	name := regexp.MustCompile(`^devvm-my-app-v2-[0-9a-f]{14}$`)
	f, err := New("/w/My_App.v2", 0)
	if err != nil || !name.MatchString(f.Name.String()) || f.Dir != "/w/My_App.v2" {
		t.Errorf("New = %+v, %v", f, err)
	}
	if f.Slug() != "my-app-v2" {
		t.Errorf("Slug = %q", f.Slug())
	}
	// Another folder with the same name gets another VM.
	if g, _ := New("/x/My_App.v2", 0); g.Name == f.Name {
		t.Errorf("two folders share %s", f.Name)
	}
	// Spaces are fine; Lima can't mount commas or control characters.
	if f, err := New("/w/My App", 0); err != nil || f.Name[:13] != "devvm-my-app-" {
		t.Errorf("New with a space = %+v, %v", f, err)
	}
	for _, dir := range []string{"/w/a,b", "/w/a\tb", "/w/a\nb", "/w/a\x1b[2Jb"} {
		var bad *BadPathError
		if _, err := New(dir, 0); !errors.As(err, &bad) || bad.Dir != dir {
			t.Errorf("New(%q) = %v, want a BadPathError", dir, err)
		}
	}
}

// A link's TARGET: a VM's name or alias, a folder name (as New makes its
// part of the VM's name, matched against the folder each VM records,
// whatever its backend cut of it), or a path, which matches the VM made
// for it whatever its backend left of the folder's name.
func TestTargets(t *testing.T) {
	long := "/w/a-very-long-folder-name-for-a-project"
	longVM, _ := New(long, 0)
	cut, _ := New(long, 30) // a backend with shorter names
	cutOther, _ := New("/v/a-very-long-folder-name-for-a-project", 30)
	web, _ := New("/a/web", 0)
	web2, _ := New("/b/web", 0)
	api, _ := New("/a/My_API", 0)
	vms := []Recorded{
		{web.Name, "devvm-web", "/a/web"},
		{web2.Name, web2.Name.String(), "/b/web"},
		{api.Name, "devvm-my-api", "/a/My_API"},
		{longVM.Name, "devvm-a-very-long-folder-n", long},
		{cutOther.Name, cutOther.Name.String(), "/v/a-very-long-folder-name-for-a-project"},
		{"devvm-img-0123456789ab", "", ""},
		{"devvm-tools-build", "", ""},
	}
	for _, tc := range []struct {
		target string
		want   []vmname.VM
	}{
		{"web", []vmname.VM{web.Name, web2.Name}},
		{web2.Name.String(), []vmname.VM{web2.Name}},
		{"devvm-web", []vmname.VM{web.Name}},
		{"devvm-my-api", []vmname.VM{api.Name}},
		{"devvm-web-00000000000000", nil},
		{"My_API", []vmname.VM{api.Name}},
		{"my-api", []vmname.VM{api.Name}},
		{"a-very-long-folder-name-for-a-project", []vmname.VM{longVM.Name, cutOther.Name}},
		{"a-very-long-folder-n", []vmname.VM{longVM.Name, cutOther.Name}},
		{"img", nil},
		{"tools", nil},
		{"devvm-tools-build", nil},
		{"nope", nil},
	} {
		if IsPath(tc.target) {
			t.Errorf("IsPath(%q) = true", tc.target)
		}
		if got := Targets(tc.target, vms); !slices.Equal(got, tc.want) {
			t.Errorf("Targets(%q) = %q, want %q", tc.target, got, tc.want)
		}
	}
	// Named matches a VM's name or alias exactly, never a folder's name.
	for target, want := range map[string]vmname.VM{
		web2.Name.String(): web2.Name, "devvm-web": web.Name, "web": "", "devvm-tools-build": "", "nope": "",
	} {
		if got := Named(target, vms); got != want {
			t.Errorf("Named(%q) = %q, want %q", target, got, want)
		}
	}
	// An alias the VM's name can't have (a hand-edited vm.json's) names
	// nothing: not another folder's, nor a word of its own.
	for _, alias := range []string{"devvm-my-api", "x"} {
		if got := Named(alias, []Recorded{{web2.Name, alias, "/b/web"}}); got != "" {
			t.Errorf("Named(%q) with web's alias %q = %q", alias, alias, got)
		}
	}
	// A VM without a recorded folder is matched by its name's folder part.
	if got := Targets("web", []Recorded{{Name: web.Name}}); !slices.Equal(got, []vmname.VM{web.Name}) {
		t.Errorf("Targets(web) without a folder = %q", got)
	}
	for _, p := range []string{"./web", "../web", "/a/web", "a/b", ".hidden"} {
		if !IsPath(p) {
			t.Errorf("IsPath(%q) = false", p)
		}
	}
	// A path matches the VM whose state records it, whatever its name: a
	// VM whose folder moved there from /m/old is found at its new path,
	// and never at the one its name was made for.
	moved, _ := New("/m/old", 0)
	all := append(slices.Clone(vms), Recorded{moved.Name, moved.Name.String(), "/m/new"})
	for dir, want := range map[string][]vmname.VM{
		"/a/web": {web.Name}, "/c/web": nil, "/m/new": {moved.Name}, "/m/old": nil, "": nil,
	} {
		if got := ForDir(dir, all); !slices.Equal(got, want) {
			t.Errorf("ForDir(%q) = %q, want %q", dir, got, want)
		}
	}
	if got := ForDir(long, []Recorded{{cut.Name, "", long}, {web.Name, "", "/a/web"}}); !slices.Equal(got, []vmname.VM{cut.Name}) {
		t.Errorf("ForDir(%s) = %q, want the VM with its name cut, %s", long, got, cut.Name)
	}
	// A VM's later names for one folder each end with a hash of their own.
	second, err := Nth("/a/web", 0, 1)
	third, _ := Nth("/a/web", 0, 2)
	if err != nil || second.Dir != "/a/web" || second.Name == web.Name || second.Name == third.Name || second.Slug() != "web" {
		t.Errorf("Nth(/a/web, 1) = %+v, %v; 2: %+v; New: %+v", second, err, third, web)
	}
	if h, _ := second.Name.Hash(); !vmname.IsHash(h) || h == Hash("/a/web") {
		t.Errorf("Nth(/a/web, 1)'s hash = %q", h)
	}
	// The hash a folder's VM is found by ends its name, whatever its
	// backend cut of the folder's name.
	if h, ok := cut.Name.Hash(); !ok || h != Hash(long) || !strings.HasSuffix(longVM.Name.String(), "-"+Hash(long)) {
		t.Errorf("Hash(%s) = %s, names %s and %s", long, Hash(long), cut.Name, longVM.Name)
	}
	if Hash("/a/web") == Hash("/b/web") || !vmname.IsHash(Hash("/a/web")) {
		t.Errorf("Hash = %s and %s", Hash("/a/web"), Hash("/b/web"))
	}
}

// A long folder name is cut to fit the backend's names; names with no
// room for four characters of it are refused.
func TestNewFitsNameMax(t *testing.T) {
	dir := "/w/a-very-long-folder-name-for-a-project"
	f, err := New(dir, 0)
	if err != nil || !regexp.MustCompile(`^devvm-a-very-long-folder-n-[0-9a-f]{14}$`).MatchString(f.Name.String()) {
		t.Errorf("New = %+v, %v", f, err)
	}
	f, err = New(dir, 30)
	if err != nil || len(f.Name) != 30 || f.Name[:15] != "devvm-a-very-lo" {
		t.Errorf("New with 30 = %+v, %v", f, err)
	}
	if _, err := New(dir, 6+4+1+14); err != nil {
		t.Errorf("New with room for four: %v", err)
	}
	if _, err := New(dir, 6+3+1+14); !errors.Is(err, ErrNoRoom) {
		t.Errorf("New with room for three = %v, want ErrNoRoom", err)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// A folder that is or holds the home folder or devvm's state, or is
// inside devvm's own folders, is unsafe; a project in the home folder isn't.
func TestUnsafe(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u := Unsafe{
		Home:   filepath.Join(root, "home"),
		Config: filepath.Join(root, "config", "devvm"),
		Data:   filepath.Join(root, "data", "devvm"),
		Lima:   filepath.Join(root, "data", "devvm", "lima"),
	}
	mkdirs(t, u.Home, u.Config, u.Lima)
	for _, tc := range []struct{ dir, holds, inside string }{
		{u.Home, u.Home, ""},
		{root, u.Home, ""},
		{"/", u.Home, ""},
		{filepath.Join(root, "data"), u.Data, ""},
		{filepath.Join(root, "config"), u.Config, ""},
		{filepath.Join(u.Config, "layers"), "", u.Config},
		{filepath.Join(u.Data, "mise"), "", u.Data},
		{filepath.Join(u.Lima, "x"), "", u.Data},
		{filepath.Join(u.Home, "proj"), "", ""},
		{u.Home + "-sibling", "", ""},
	} {
		if got := u.Holds(tc.dir); got != tc.holds {
			t.Errorf("Holds(%s) = %q, want %q", tc.dir, got, tc.holds)
		}
		if got := u.Inside(tc.dir); got != tc.inside {
			t.Errorf("Inside(%s) = %q, want %q", tc.dir, got, tc.inside)
		}
	}
	// Through a symlink, as given: the folders are compared by their real paths.
	link := filepath.Join(root, "link")
	if err := os.Symlink(u.Home, link); err != nil {
		t.Fatal(err)
	}
	if got := (Unsafe{Home: link}).Holds(u.Home); got != link {
		t.Errorf("Holds through a link = %q", got)
	}
}

// Parents lists the folders a folder is inside, nearest first, stopping
// before the home folder (given through a symlink too) and the root.
func TestParents(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	mkdirs(t, home)
	link := filepath.Join(root, "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir, home string
		want      []string
	}{
		{filepath.Join(home, "app", "src", "lib"), home, []string{filepath.Join(home, "app", "src"), filepath.Join(home, "app")}},
		{filepath.Join(home, "app", "src"), link, []string{filepath.Join(home, "app")}},
		{filepath.Join(home, "app"), home, nil},
		{home, home, nil},
		{"/srv/app/src", home, []string{"/srv/app", "/srv"}},
		{"/srv", home, nil},
		{"/", home, nil},
	} {
		if got := Parents(tc.dir, tc.home); !slices.Equal(got, tc.want) {
			t.Errorf("Parents(%s, %s) = %q, want %q", tc.dir, tc.home, got, tc.want)
		}
	}
}
