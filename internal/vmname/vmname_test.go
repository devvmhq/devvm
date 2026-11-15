package vmname

import (
	"encoding/json"
	"testing"
)

func TestIsVM(t *testing.T) {
	for name, want := range map[string]bool{
		"devvm-web-0123456789abcd":        true,
		"devvm-my-app-0123456789abcd":     true,
		"devvm-img-x-0123456789abcd":      true, // a folder named img-x
		"devvm-a-b-0123456789abcd":        true,
		"devvm-tools-build":               false,
		"devvm-web-123456":                false,
		"other-web-12345612345612":        false,
		"devvm-img-0123456789ab":          false, // an image: 12 hex digits
		"devvm-web-0123456789abc":         false,
		"devvm-web-0123456789abcde":       false,
		"devvm-web-0123456789ABCD":        false,
		"devvm--0123456789abcd":           false,
		"devvm-0123456789abcd":            false,
		"devvm-w\neb-0123456789abcd":      false,
		"devvm-a--b-0123456789abcd":       true, // project.Slug keeps each dash
		"devvm-Web-0123456789abcd":        false,
		"devvm-w.b-0123456789abcd":        false,
		"devvm-w b-0123456789abcd":        false,
		"devvm-w/b-0123456789abcd":        false,
		"devvm-w_b-0123456789abcd":        false,
		"devvm-wé-0123456789abcd":         false,
		"devvm--web-0123456789abcd":       false,
		"devvm-web--0123456789abcd":       false,
		"devvm-w;b-0123456789abcd":        false,
		"devvm-w*b-0123456789abcd":        false,
		"xdevvm-web-0123456789abcd":       false,
		"devvm-web-0123456789abcd\n":      false,
		"devvm-web-0123456789abcd.tar.gz": false,
		"":                                false,
		"base":                            false,
	} {
		if got := IsVM(name); got != want {
			t.Errorf("IsVM(%q) = %v, want %v", name, got, want)
		}
		v, err := Parse(name)
		if (err == nil) != want || want && v.String() != name || VM(name).IsProject() != want {
			t.Errorf("Parse(%q) = %q, %v", name, v, err)
		}
	}
}

// A backend's list gives any name that's one path element.
func TestParseListed(t *testing.T) {
	for name, want := range map[string]bool{
		"devvm-web-0123456789abcd": true,
		"devvm-img-0123456789ab":   true,
		"devvm-tools-build":        true,
		"devvm-0123456789abcd-old": true,
		"someone-else":             true,
		"":                         false,
		".":                        false,
		"..":                       false,
		"a/b":                      false,
		"a\x00b":                   false,
		"-x":                       false,
		"--help":                   false,
		"a-":                       true,
	} {
		if v, err := ParseListed(name); (err == nil) != want || want && string(v) != name {
			t.Errorf("ParseListed(%q) = %q, %v", name, v, err)
		}
	}
}

// A VM in a JSON file is read as ParseListed reads it, map keys too, and
// written as the name itself.
func TestJSON(t *testing.T) {
	var got struct {
		VM   VM          `json:"vm"`
		None VM          `json:"none"`
		By   map[VM]bool `json:"by"`
	}
	in := `{"vm":"devvm-web-0123456789abcd","none":"","by":{"devvm-tools-build":true}}`
	if err := json.Unmarshal([]byte(in), &got); err != nil || got.VM != "devvm-web-0123456789abcd" || got.None != "" || !got.By["devvm-tools-build"] {
		t.Fatalf("Unmarshal = %+v, %v", got, err)
	}
	if out, _ := json.Marshal(got); string(out) != in {
		t.Errorf("Marshal = %s", out)
	}
	for _, bad := range []string{`{"vm":"../x"}`, `{"by":{"a/b":true}}`, `{"vm":".."}`, `{"vm":"--help"}`} {
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("Unmarshal(%s): no error", bad)
		}
	}
}

// An image's machine is devvm-img-<key>, which a folder named img-…
// doesn't make.
func TestImage(t *testing.T) {
	v, err := Image("0123456789ab")
	if err != nil || v != "devvm-img-0123456789ab" {
		t.Fatalf("Image = %q, %v", v, err)
	}
	// A key that makes no image's machine, nor a path's one element.
	for _, key := range []string{"", "../x", "a/b", "a\x00b", "x-0123456789abcd"} {
		if v, err := Image(key); err == nil {
			t.Errorf("Image(%q) = %q", key, v)
		}
	}
	if k, ok := v.ImageKey(); !ok || k != "0123456789ab" {
		t.Errorf("ImageKey = %q, %v", k, ok)
	}
	for _, s := range []VM{"devvm-img-x-0123456789abcd", "devvm-img-", "devvm-web-0123456789abcd", "devvm-tools-build"} {
		if k, ok := s.ImageKey(); ok {
			t.Errorf("%s: ImageKey = %q", s, k)
		}
	}
}

func TestHash(t *testing.T) {
	for _, c := range []struct {
		name VM
		hash string
		ok   bool
	}{
		{"devvm-web-0123456789abcd", "0123456789abcd", true},
		{"devvm-a-b-0123456789abcd", "0123456789abcd", true},
		{"devvm-img-0123456789ab", "", false},
		{"devvm-tools-build", "", false},
		{"0123456789abcd", "", false},
		{"", "", false},
	} {
		if h, ok := c.name.Hash(); h != c.hash || ok != c.ok {
			t.Errorf("Hash(%q) = %q, %v; want %q, %v", c.name, h, ok, c.hash, c.ok)
		}
		if c.ok && !IsHash(c.hash) {
			t.Errorf("IsHash(%q) = false", c.hash)
		}
	}
	for _, s := range []string{"", "0123456789abc", "0123456789abcde", "0123456789ABCD", "devvm-web-0123456789abcd", "../123456789abc"} {
		if IsHash(s) {
			t.Errorf("IsHash(%q) = true", s)
		}
	}
}

func TestSlug(t *testing.T) {
	for _, c := range []struct {
		name VM
		slug string
		ok   bool
	}{
		{"devvm-web-0123456789abcd", "web", true},
		{"devvm-my-app-0123456789abcd", "my-app", true},
		{"devvm-img-x", "img", true},
		{"devvm-img-0123456789ab", "img", true},
		{"devvm-web", "", false},
		{"devvm--x", "", false},
		{"devvm-", "", false},
		{"other", "", false},
		{"other-x-0123456789abcd", "", false},
	} {
		if slug, ok := c.name.Slug(); slug != c.slug || ok != c.ok {
			t.Errorf("Slug(%q) = %q, %v; want %q, %v", c.name, slug, ok, c.slug, c.ok)
		}
	}
}

// A VM's alias is its short name or its name, nothing else.
func TestIsAlias(t *testing.T) {
	const vm VM = "devvm-my-app-0123456789abcd"
	if s, ok := vm.Short(); !ok || s != "devvm-my-app" {
		t.Errorf("Short(%s) = %q, %v", vm, s, ok)
	}
	if _, ok := VM("devvm-tools-build").Short(); ok {
		t.Error("a short name for a build VM")
	}
	for alias, want := range map[string]bool{
		"devvm-my-app":                true,
		string(vm):                    true,
		"devvm-my":                    false,
		"devvm-my-app-x":              false,
		"my-app":                      false,
		"*":                           false,
		"":                            false,
		"devvm-my-app\n  ProxyJump x": false,
		"devvm-other-0123456789abcd":  false,
	} {
		if got := IsAlias(alias, vm); got != want {
			t.Errorf("IsAlias(%q) = %v, want %v", alias, got, want)
		}
	}
	if IsAlias("devvm-tools", "devvm-tools-build") || IsAlias("devvm-tools-build", "devvm-tools-build") {
		t.Error("an alias for a build VM")
	}
}

// A VM's old machine, set aside while a remake makes the new one, has a
// name that's neither a VM's nor an image's, which AsideHash reads, and
// is longer than the shortest VM name by two.
func TestAside(t *testing.T) {
	a, ok := VM("devvm-my-app-0123456789abcd").Aside()
	if !ok || a != "devvm-0123456789abcd-old" {
		t.Fatalf("Aside = %q, %v", a, ok)
	}
	if h, ok := a.AsideHash(); a.IsProject() || !ok || h != "0123456789abcd" {
		t.Errorf("%s: IsProject %v, AsideHash %q, %v", a, a.IsProject(), h, ok)
	}
	if shortest := "devvm-x-0123456789abcd"; len(a) != len(shortest)+2 || !IsVM(shortest) {
		t.Errorf("%s is %d long", a, len(a))
	}
	if _, ok := VM("devvm-tools-build").Aside(); ok {
		t.Error("an aside name for a build VM")
	}
	for _, s := range []VM{"devvm-0123456789abc-old", "devvm-0123456789abcd", "x-0123456789abcd-old", "devvm-0123456789abcd-old-x", "devvm-web-0123456789abcd"} {
		if _, ok := s.AsideHash(); ok {
			t.Errorf("AsideHash(%q) is one", s)
		}
	}
}
