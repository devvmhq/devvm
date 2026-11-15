package redact

import (
	"bytes"
	"errors"
	"io"
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
		// A password at its bound is found; one past it isn't a password the patterns know.
		{"http://u:" + strings.Repeat("p", 512) + "@h", "http://u:<password>@h"},
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

// In overwrites each secret with as many stars in place, in a real file a writer still has open
// at its own offset, as Lima's hostagent does: the file keeps its length, and the writer goes on
// where it was.
func TestInFile(t *testing.T) {
	t.Parallel()
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	key := strings.Repeat("fedcba9876543210", 4)
	path := filepath.Join(t.TempDir(), "ha.stderr.log")
	before := `level=info msg="Replacing \"HTTPS_PROXY\" value ` + "`http://alice:" + pass + "@127.0.0.1:3128`" + `"` + "\n" +
		"proxy http://devvm:" + key + "@192.168.5.2:3129 kept\nno user https://host:443/a@b\n"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(before); err != nil {
		t.Fatal(err)
	}
	at, err := In(f, int64(len(before)), Look{}, 1<<20)
	if err != nil || at != (Look{Line: int64(len(before)), Read: int64(len(before))}) {
		t.Fatalf("look %+v, %v", at, err)
	}
	if _, err := f.WriteString("later\n"); err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(pass, strings.Repeat("*", len(pass)), key, strings.Repeat("*", len(key))).Replace(before) + "later\n"
	if b, _ := os.ReadFile(path); string(b) != want { //nolint:gosec // the test's own file
		t.Errorf("got  %q\nwant %q", b, want)
	}
}

// counting is a log in memory that counts what's read of it.
type counting struct {
	Bytes
	read int
}

func (c *counting) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.Bytes.ReadAt(p, off)
	c.read += n
	return n, err
}

// In redacts a log as it grows: each look takes the lines from where the last one left off, so
// a secret the writer hadn't ended yet is found once its line is, one written again later is
// found too, and a log emptied and started again is read from its start.
func TestIn(t *testing.T) {
	t.Parallel()
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	log := Bytes("[  OK  ] boot\n+ echo HTTPS_PROXY=http://devvm:" + pass[:5])
	at, err := In(log, int64(len(log)), Look{}, 1<<20)
	if err != nil || at.Line != int64(len("[  OK  ] boot\n")) || at.Read != int64(len(log)) {
		t.Fatalf("look %+v, %v", at, err)
	}
	log = append(log, pass[5:]+"@192.168.5.2:3129\nlater\n"...)
	if at, err = In(log, int64(len(log)), at, 1<<20); err != nil || at.Line != int64(len(log)) {
		t.Fatalf("look %+v of %d, %v", at, len(log), err)
	}
	if strings.Contains(string(log), pass) || !strings.Contains(string(log), strings.Repeat("*", len(pass))) {
		t.Errorf("after the line ended: %q", log)
	}
	// A reboot writes it again, past where the last look ended.
	log = append(log, "+ echo HTTPS_PROXY=http://devvm:"+pass+"@192.168.5.2:3129\n"...)
	if _, err := In(log, int64(len(log)), at, 1<<20); err != nil || strings.Contains(string(log), pass) {
		t.Errorf("written again: %q, %v", log, err)
	}
	// Emptied and started again: from its start.
	log = Bytes("x http://a:" + pass + "@h:1\n")
	if _, err := In(log, int64(len(log)), Look{Line: 1 << 10, Read: 1 << 10}, 1<<20); err != nil || strings.Contains(string(log), pass) {
		t.Errorf("started again: %q, %v", log, err)
	}
	// At most limit bytes a look, the next going on from there.
	log = Bytes(strings.Repeat("y", 100))
	if at, err := In(log, int64(len(log)), Look{}, 10); err != nil || at != (Look{Read: 10}) {
		t.Errorf("limited: %+v, %v", at, err)
	}
}

// A line that never ends is read once, not at every look: a look reads again only MatchMax of
// what came before, so a VM can't make each look cost its whole line, and a secret whose "@"
// comes in what's new is still found whole.
func TestInLongLine(t *testing.T) {
	t.Parallel()
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	c := &counting{Bytes: Bytes("@" + strings.Repeat("x", 1<<20) + " http://u:" + pass)}
	at, err := In(c, int64(len(c.Bytes)), Look{}, 64<<20)
	if err != nil || at.Line != 0 {
		t.Fatalf("look %+v, %v", at, err)
	}
	c.read = 0
	if again, err := In(c, int64(len(c.Bytes)), at, 64<<20); err != nil || again != at || c.read > MatchMax {
		t.Errorf("a still line: look %+v, read %d bytes, %v", again, c.read, err)
	}
	c.Bytes = append(c.Bytes, "@proxy:3128"...)
	c.read = 0
	if _, err := In(c, int64(len(c.Bytes)), at, 64<<20); err != nil || c.read > MatchMax+20 || strings.Contains(string(c.Bytes), pass) {
		t.Errorf("its end: read %d bytes, %v, ends %q", c.read, err, c.Bytes[len(c.Bytes)-40:])
	}
}

// Long secrets, such as a registry's token in an index URL, are found whatever their length: by
// String in the run log, and by In in a log that grows, the line's end coming at a later look.
func TestLongSecrets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ user, pass int }{{5, 513}, {5, 1000}, {5, 2048}, {600, 40}} {
		user, pass := strings.Repeat("u", tc.user), strings.Repeat("p", tc.pass-1)+"Z"
		line := "index-url = https://" + user + ":" + pass + "@pkgs.example/simple\n"
		if got := String(line); strings.Contains(got, pass) || !strings.Contains(got, user+":"+passwordStandIn+"@") {
			t.Errorf("String, user %d, password %d: %.80q", tc.user, tc.pass, got)
		}
		log := Bytes(line[:len(line)-len("@pkgs.example/simple\n")])
		at, err := In(log, int64(len(log)), Look{}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, "@pkgs.example/simple\n"...)
		if _, err := In(log, int64(len(log)), at, 1<<20); err != nil || strings.Contains(string(log), pass) {
			t.Errorf("In, user %d, password %d: %v", tc.user, tc.pass, err)
		}
	}
}

// A look that reads a long line from MatchMax before where the last one stopped, past the start
// of a secret, stars the rest of it up to its "@", as it does what a rotation left after a cut
// (Look.Cut); a run that the "@" doesn't end is left, and one still open is looked at again.
func TestInLeading(t *testing.T) {
	t.Parallel()
	pass := strings.Repeat("q", MatchMax+100)
	log := Bytes("@" + strings.Repeat("x", 2*MatchMax) + " http://u:" + pass)
	at, err := In(log, int64(len(log)), Look{}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	log = append(log, "@h\n"...)
	if _, err := In(log, int64(len(log)), at, 1<<30); err != nil || strings.Contains(string(log), pass[:MatchMax]) {
		t.Errorf("past MatchMax: %v, the password's last part left", err)
	}
	// After a cut: zeros of a hole, then a key's rest, still open, then ended.
	tail := "\x00\x00" + "89abcdef"
	log = Bytes(tail)
	at, err = In(log, int64(len(log)), Look{Cut: true}, 1<<20)
	if err != nil || !at.Cut || string(log) != tail {
		t.Fatalf("open: %+v, %v, %q", at, err, log)
	}
	log = append(log, "@192.168.5.2:3129\nnext\n"...)
	if at, err = In(log, int64(len(log)), at, 1<<20); err != nil || at.Cut || strings.Contains(string(log), "89abcdef") {
		t.Errorf("ended: %+v, %v, %q", at, err, log)
	}
	// A run that something else ends isn't a secret's rest.
	log = Bytes("plain words@ and more\n")
	if _, err := In(log, int64(len(log)), Look{Cut: true}, 1<<20); err != nil || string(log) != "plain words@ and more\n" {
		t.Errorf("a word: %q", log)
	}
}

// Unended masks the start of a secret at a copy's cut end, which the patterns can't tell yet;
// a copy that ends a line is left as it is.
func TestUnended(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{ //nolint:gosec // fake passwords
		"a\nproxy http://devvm:0123abcd":   "a\nproxy http://devvm:********",
		"a\nHTTPS_PROXY=http://alice:s3cr": "a\nHTTPS_PROXY=http://alice:****",
		"a\nhttp://devvm:0123abcd@h:1\n":   "a\nhttp://devvm:0123abcd@h:1\n",
		"plain text, cut":                  "plain text, cut",
		"":                                 "",
	} {
		b := []byte(in)
		if Unended(b); string(b) != want {
			t.Errorf("Unended(%q) = %q, want %q", in, b, want)
		}
	}
}

// Bytes reads and writes within itself, as a file of its length does.
func TestBytes(t *testing.T) {
	t.Parallel()
	b := Bytes("abc")
	p := make([]byte, 5)
	if n, err := b.ReadAt(p, 1); n != 2 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt past the end: %d, %v", n, err)
	}
	if n, err := b.ReadAt(p[:2], 0); n != 2 || err != nil {
		t.Errorf("ReadAt: %d, %v", n, err)
	}
	if n, err := b.ReadAt(p, 3); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end: %d, %v", n, err)
	}
	if _, err := b.WriteAt([]byte("xyz"), 1); err == nil {
		t.Error("WriteAt past the end")
	}
	if _, err := b.WriteAt([]byte("x"), 1); err != nil || string(b) != "axc" {
		t.Errorf("WriteAt: %q, %v", b, err)
	}
}

type failingWriterAt struct{}

func (failingWriterAt) WriteAt([]byte, int64) (int, error) { return 0, os.ErrPermission }

// A write that fails stops the overwrite with its error.
func TestOverwriteFails(t *testing.T) {
	t.Parallel()
	if err := overwrite(offsetWriter{failingWriterAt{}, 5}, [][2]int{{0, 3}}); err == nil {
		t.Error("no error")
	}
	if _, err := In(struct {
		io.ReaderAt
		io.WriterAt
	}{Bytes("http://a:b@h\n"), failingWriterAt{}}, 13, Look{}, 100); err == nil {
		t.Error("In: no error")
	}
}
