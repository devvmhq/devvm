package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

func TestParseExportName(t *testing.T) {
	for _, c := range []struct {
		name  string
		vm    vmname.VM
		stamp string
		ok    bool
	}{
		{"devvm-web-0123456789abcd-20260101-120000.tar.gz", "devvm-web-0123456789abcd", "20260101-120000", true},
		{"devvm-img-keep-0123456789abcd-20260101-120000.tar.gz", "devvm-img-keep-0123456789abcd", "20260101-120000", true},
		{"devvm-img-0123456789ab-20260101-120000.tar.gz", "", "", false},
		{"devvm-web-0123456789abcd-2026010-120000.tar.gz", "", "", false},
		{"devvm-web-0123456789abcd-2026010a-120000.tar.gz", "", "", false},
		{"devvm-web-0123456789abcd-20260101x120000.tar.gz", "", "", false},
		{"devvm-web-0123456789abcd-20260101-120000.tar", "", "", false},
		{"devvm-web-0123456789abcd.tar.gz", "", "", false},
		{"-20260101-120000.tar.gz", "", "", false},
		{".devvm-export-1", "", "", false},
	} {
		if vm, stamp, ok := parseExportName(c.name); vm != c.vm || stamp != c.stamp || ok != c.ok {
			t.Errorf("parseExportName(%q) = %q, %q, %v", c.name, vm, stamp, ok)
		}
	}
}

// A new export's name is one List reads back, as written.
func TestExportNewFile(t *testing.T) {
	x := Exports{Dirs: paths.Dirs{Data: "/d"}}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local)
	f := x.newFile("devvm-web-0123456789abcd", at)
	if f != "/d/exports/devvm-web-0123456789abcd-20260304-050607.tar.gz" {
		t.Fatalf("newFile = %s", f)
	}
	if vm, stamp, ok := parseExportName(filepath.Base(f)); !ok || vm != "devvm-web-0123456789abcd" || stamp != "20260304-050607" {
		t.Errorf("parsed %q, %q, %v", vm, stamp, ok)
	}
}

// writeExports writes these files in the exports folder, each with a
// .folder file naming folder when it isn't "".
func writeExports(t *testing.T, d paths.Dirs, folders map[string]string) {
	t.Helper()
	if err := os.MkdirAll(d.Exports(), 0o700); err != nil {
		t.Fatal(err)
	}
	x := Exports{Dirs: d}
	for f, dir := range folders {
		p := filepath.Join(d.Exports(), f)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if dir != "" {
			if err := x.writeFolder(p, dir, netpolicy.TierAllowlist); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The file beside an export: its folder, then its VM's tier, which it
// reads back; one without a tier, or with one that isn't a tier, has the
// folder alone, and none has neither.
func TestExportRecord(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	x := Exports{Dirs: d}
	p := filepath.Join(t.TempDir(), "devvm-a-12345612345612-20260101-120000.tar.gz")
	if err := x.writeFolder(p, "/p", netpolicy.TierNone); err != nil {
		t.Fatal(err)
	}
	if dir, tier := x.record(p); dir != "/p" || tier != netpolicy.TierNone {
		t.Errorf("record = %q, %q", dir, tier)
	}
	for data, want := range map[string]string{"/p\n": "/p", "/p\nwide\n": "/p", "/p": "/p", "": ""} {
		if err := os.WriteFile(paths.ExportFolder(p), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if dir, tier := x.record(p); dir != want || tier != "" {
			t.Errorf("record of %q = %q, %q", data, dir, tier)
		}
	}
	if dir, tier := x.record(filepath.Join(t.TempDir(), "none.tar.gz")); dir != "" || tier != "" {
		t.Errorf("record of none = %q, %q", dir, tier)
	}
}

// Exports: only plain files named as devvm names them, oldest first, each
// with its folder; the age filter keeps those at least that old; Newest
// is a folder's newest; Delete takes the .folder file too.
func TestExports(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	writeExports(t, d, map[string]string{
		"devvm-b-12345612345612-20260301-000000.tar.gz": "/p", "devvm-a-12345612345612-20260101-120000.tar.gz": "/p",
		"devvm-c-12345612345613-20260401-000000.tar.gz": "/q",
		".devvm-export-1": "", "notes.tar.gz": "", "devvm-c-1234-20260101-000000.tar.gz": "",
	})
	if err := os.Mkdir(filepath.Join(d.Exports(), "devvm-d-12345612345612-20260101-000000.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	x := Exports{Dirs: d}
	es, err := x.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 || es[0].VM != "devvm-a-12345612345612" || es[1].VM != "devvm-b-12345612345612" || es[0].Folder != "/p" || es[0].Size <= 0 {
		t.Fatalf("exports = %+v", es)
	}
	if ExportsSize(es) != es[0].Size+es[1].Size+es[2].Size {
		t.Errorf("ExportsSize = %d", ExportsSize(es))
	}
	// Now is counted from b's parsed date, so the zone the test runs in
	// doesn't matter: exactly 30d after b both are that old, a second
	// short only a.
	age := 30 * 24 * time.Hour
	boundary := es[1].At.Add(age)
	if got := ExportsOlderThan(es, age, boundary); len(got) != 2 {
		t.Errorf("olderThan 30d, at b's 30d = %+v", got)
	}
	if got := ExportsOlderThan(es, age, boundary.Add(-time.Second)); len(got) != 1 || got[0].VM != "devvm-a-12345612345612" {
		t.Errorf("olderThan 30d, a second short = %+v", got)
	}
	if got := ExportsOlderThan(es, 0, boundary); len(got) != 3 {
		t.Errorf("olderThan 0 = %+v", got)
	}
	if e, ok, err := x.Newest("/p"); err != nil || !ok || e.VM != "devvm-b-12345612345612" {
		t.Errorf("Newest(/p) = %+v, %v, %v", e, ok, err)
	}
	if _, ok, err := x.Newest("/r"); err != nil || ok {
		t.Errorf("Newest(/r) = %v, %v", ok, err)
	}
	if err := x.Delete(es[:1]); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{es[0].Path, paths.ExportFolder(es[0].Path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %v, want it gone", p, err)
		}
	}
	if left, _ := x.List(); len(left) != 2 {
		t.Errorf("left %+v", left)
	}
}

// No exports folder: none, and no failure.
func TestExportsNone(t *testing.T) {
	x := Exports{Dirs: paths.Dirs{Data: t.TempDir()}}
	if es, err := x.List(); err != nil || len(es) != 0 {
		t.Errorf("List = %+v, %v", es, err)
	}
	if _, ok, err := x.Newest("/p"); ok || err != nil {
		t.Errorf("Newest = %v, %v", ok, err)
	}
}

// history.age deletes those that old; history.count then keeps the newest;
// off deletes none.
func TestExportsToRetire(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	es := []Export{{VM: "a", At: now.Add(-40 * day)}, {VM: "b", At: now.Add(-20 * day)}, {VM: "c", At: now.Add(-10 * day)}, {VM: "d", At: now}}
	vms := func(es []Export) string {
		s := ""
		for _, e := range es {
			s += e.VM.String()
		}
		return s
	}
	for _, tt := range []struct {
		r    settings.Retention
		want string
	}{
		{settings.Retention{}, ""},
		{settings.Retention{ExportsAge: 30 * day}, "a"},
		{settings.Retention{ExportsCount: 2}, "ab"},
		{settings.Retention{ExportsAge: 30 * day, ExportsCount: 2}, "ab"},
		{settings.Retention{ExportsAge: 15 * day, ExportsCount: 1}, "abc"},
		{settings.Retention{ExportsCount: 9}, ""},
	} {
		if got := vms(ExportsToRetire(es, tt.r, now)); got != tt.want {
			t.Errorf("%+v: got %q, want %q", tt.r, got, tt.want)
		}
	}
}
