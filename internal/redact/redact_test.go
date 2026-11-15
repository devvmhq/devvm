package redact

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	t.Parallel()
	key := strings.Repeat("0123456789abcdef", 2)
	for _, tc := range []struct{ in, want string }{
		{"plain line", "plain line"},
		{"http://devvm:" + key + "@host:3128", "http://devvm:<key>@host:3128"},
		{"HTTPS_PROXY=http://alice:s3cr%40t@proxy.corp:8080", "HTTPS_PROXY=http://alice:<password>@proxy.corp:8080"},
		{"value `http://bob:pw@127.0.0.1:3128` and \"https://c:d@e\"", "value `http://bob:<password>@127.0.0.1:3128` and \"https://c:<password>@e\""},
		{"no user https://host:443/a@b", "no user https://host:443/a@b"},
		{"user only http://alice@proxy", "user only http://alice@proxy"},
		// A proxy URL without a scheme, with its port.
		{"HTTPS_PROXY=alice:s3cr%40t@proxy.corp:8080", "HTTPS_PROXY=alice:<password>@proxy.corp:8080"},
		{"proxy alice:pw@10.0.0.1:3128/ and 'bob:p:w@[::1]:80'", "proxy alice:<password>@10.0.0.1:3128/ and 'bob:<password>@[::1]:80'"},
		{"devvm:" + key + "@192.168.5.2:3129", "devvm:<key>@192.168.5.2:3129"},
		// Ordinary text with an "@" is left alone: ssh targets, emails, a user without a password.
		{"git@github.com:org/repo.git and ssh alice@host -p 22", "git@github.com:org/repo.git and ssh alice@host -p 22"},
		{"mailto:bob@example.com, Contact: carol@example.com:25", "mailto:bob@example.com, Contact: carol@example.com:25"},
		{"alice@proxy.corp:8080 at 10:30", "alice@proxy.corp:8080 at 10:30"},
	} {
		if got := String(tc.in); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Split ends what a writer writes of a line before its last word, at most tail bytes before its
// end, and never inside a secret.
func TestSplit(t *testing.T) {
	t.Parallel()
	long := "http://alice:e2e-not-a-secret@proxy:3128" //nolint:gosec // a fake password
	for _, tc := range []struct {
		in   string
		tail int
		want int
	}{
		{"", 8, 0},
		{"no end http://bob:e2e-no", 64, len("no end ")},
		{"tab\tend", 64, len("tab\t")},
		{"noword", 64, 0},
		{"noword", 2, 4},
		// A secret the tail would cut is held whole.
		{`"` + long + `"`, len(`secret@proxy:3128"`), 1},
		{"x=alice:e2e-not-a-secret@proxy:3128", len("secret@proxy:3128"), 1},
		{"x " + long, 64, 2},
	} {
		if got := Split([]byte(tc.in), tc.tail); got != tc.want {
			t.Errorf("Split(%q, %d) = %d, want %d", tc.in, tc.tail, got, tc.want)
		}
	}
}

// splitWriter is how a writer uses Split, as devvm's run log does: what comes is held; each line
// that ends is written whole, redacted; and at each of the writer's flush points it writes the
// line still to come as far as Split lets it, redacted, holding the rest. At the end it writes
// what it holds.
func splitWriter(line string, sizes []byte, tail int) string {
	var out strings.Builder
	var buf []byte
	for len(line) > 0 {
		n := 1
		if len(sizes) > 0 {
			n, sizes = int(sizes[0])+1, sizes[1:]
		}
		n = min(n, len(line))
		buf, line = append(buf, line[:n]...), line[n:]
		if i := bytes.LastIndexAny(buf, "\n\r"); i >= 0 {
			out.WriteString(String(string(buf[:i+1])))
			buf = buf[i+1:]
		}
		i := Split(buf, tail)
		out.WriteString(String(string(buf[:i])))
		buf = buf[i:]
	}
	out.WriteString(String(string(buf)))
	return out.String()
}

// FuzzSplit: a password that String finds in a line whole, in a URL of up to tail bytes, isn't
// written by a writer that writes the line in parts, however it comes and wherever Split lets it
// cut, with a flush after every part.
func FuzzSplit(f *testing.F) {
	const pw = "Pw0rdZ9qXk" //nolint:gosec // a fake password
	for _, s := range [][3]string{
		{"HTTPS_PROXY=", "alice:", "@proxy.corp:8080 next"},
		{"get ", "http://alice:", "@proxy:3128/x"},
		{`"`, "https://u:", `@h:1"`},
		{"x ", "devvm:", "@127.0.0.1:3128"},
		{"\n", "bob:", "@[::1]:80\r"},
		{"x ", "http://alice:", "@proxy:3128/" + strings.Repeat("a", 300)},
		{"=", "alice:", "@proxy.corp:8080" + strings.Repeat("b", 300)},
	} {
		f.Add(s[0], s[1], s[2], []byte{3, 7, 20}, uint16(64))
		f.Add(s[0], s[1], s[2], []byte{0}, uint16(8))
	}
	f.Fuzz(func(t *testing.T, before, url, after string, sizes []byte, tailN uint16) {
		line := before + url + pw + after
		tail := int(tailN)%256 + 1
		// Split itself: in every start of the line, it cuts outside every secret already whole there.
		for n := range min(len(line), 2048) + 1 { // each start: quadratic, so the first 2 KiB
			b := []byte(line[:n])
			i := Split(b, tail)
			if i < 0 || i > n {
				t.Fatalf("Split(%q, %d) = %d", b, tail, i)
			}
			for _, re := range secrets() {
				for _, m := range re.FindAllIndex(b, -1) {
					if m[0] < i && i < m[1] {
						t.Fatalf("Split(%q, %d) = %d, inside %q", b, tail, i, b[m[0]:m[1]])
					}
				}
			}
		}
		if strings.Count(line, pw) != 1 || strings.Contains(String(line), pw) {
			return // the password twice, or one String doesn't find whole
		}
		at := strings.Index(line, pw)
		within := false
		for _, re := range secrets() {
			for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
				within = within || m[len(m)-2] <= at && at+len(pw) <= m[len(m)-1] && m[1]-m[0] <= tail
			}
		}
		if !within {
			return // in a URL longer than tail: Split cuts it, as it says
		}
		if out := splitWriter(line, sizes, tail); strings.Contains(out, pw) {
			t.Fatalf("written in parts of %v with tail %d, the password survives: %q", sizes, tail, out)
		}
	})
}

// File overwrites each secret with as many stars in place, so the file keeps its length, and a
// writer that still has it open at its own offset, as Lima's hostagent does, goes on where it was.
func TestFile(t *testing.T) {
	t.Parallel()
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	key := strings.Repeat("fedcba9876543210", 4)
	path := filepath.Join(t.TempDir(), "ha.stderr.log")
	before := `level=info msg="Replacing \"HTTPS_PROXY\" value ` + "`http://alice:" + pass + "@127.0.0.1:3128`" + `"` + "\n" +
		"proxy http://devvm:" + key + "@192.168.5.2:3129 kept\nno user https://host:443/a@b\n"
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.WriteString(before); err != nil {
		t.Fatal(err)
	}
	if err := File(path); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("later\n"); err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(pass, strings.Repeat("*", len(pass)), key, strings.Repeat("*", len(key))).Replace(before) + "later\n"
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("got  %q\nwant %q", b, want)
	}
	// Once more changes nothing; a file without secrets, or none, is left alone.
	if err := File(path); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(path); string(b2) != want { //nolint:gosec // the test's own file
		t.Errorf("again: %q", b2)
	}
	if err := File(filepath.Join(t.TempDir(), "missing.log")); err != nil {
		t.Errorf("a missing file: %v", err)
	}
	if err := File(t.TempDir()); err == nil {
		t.Error("a folder: no error")
	}
}
