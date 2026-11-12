package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var nasty = []string{
	"", "plain", "two words", "it's", `"double"`, `back\slash`, "$HOME", "$(touch /tmp/x)", "`id`",
	"a;b", "a&&b", "a|b", "*", "~", "!x", "new\nline", "tab\there", "'", "''", `'\''`, "-n", "--", "é ü 漢",
	"\x01\x7f",
}

// sh runs script with sh -c and returns its output.
func sh(t *testing.T, script string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "sh", "-c", script).Output() //nolint:gosec // quoting is what's tested
	return string(out), err
}

func TestQuoteText(t *testing.T) {
	for in, want := range map[string]string{
		"":      "''",
		"plain": "'plain'",
		"it's":  `'it'\''s'`,
		"'":     `''\'''`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := QuoteJoin(nil); got != "" {
		t.Errorf("QuoteJoin(nil) = %q", got)
	}
	if got := QuoteJoin([]string{"a b", "it's"}); got != `'a b' 'it'\''s'` {
		t.Errorf("QuoteJoin = %s", got)
	}
}

// What Quote quotes reaches the shell's command as it was.
func TestQuote(t *testing.T) {
	for _, s := range nasty {
		if out, err := sh(t, "printf %s "+Quote(s)); err != nil || out != s {
			t.Errorf("Quote(%q): the shell got %q, %v", s, out, err)
		}
	}
	out, err := sh(t, `printf '%s\0' `+QuoteJoin(nasty))
	if err != nil || out != strings.Join(nasty, "\x00")+"\x00" {
		t.Errorf("QuoteJoin: the shell got %q, %v", out, err)
	}
}

// bashes are the bash programs here: macOS's /bin/bash is 3.2, the oldest
// devvm's scripts run with, and PATH's may be newer.
func bashes(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, b := range []string{"/bin/bash", "bash"} {
		if p, err := exec.LookPath(b); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		t.Skip("no bash")
	}
	return found
}

// locales are the C locale and those of the multibyte locales this
// machine has, whose characters can end in a backslash's or a quote's
// byte, so a lead byte at or above 0x80 could swallow the backslash that
// escapes a quote.
func locales(t *testing.T) []string {
	t.Helper()
	found := []string{"C"}
	out, _ := exec.CommandContext(t.Context(), "locale", "-a").Output()
	for _, l := range []string{"ja_JP.SJIS", "zh_CN.GBK", "zh_CN.GB18030", "zh_TW.Big5"} {
		if slices.ContainsFunc(strings.Fields(string(out)), func(have string) bool { return strings.EqualFold(have, l) }) {
			found = append(found, l)
		}
	}
	return found
}

// What QuoteLine quotes is one line of ASCII, and each bash reads it back
// as it was in each locale: every byte but NUL, the nasty words, and a
// word that breaks out where a multibyte lead byte goes through raw.
func TestQuoteLine(t *testing.T) {
	every := make([]byte, 0, 255)
	for c := 1; c < 256; c++ {
		every = append(every, byte(c))
	}
	ran := filepath.Join(t.TempDir(), "ran")
	out := "\x95\\'; touch " + ran + "; echo '\x95\\"
	words := append([]string{string(every), "a\\'b\\\\'\n'\n", out}, nasty...)
	for _, s := range words {
		q := QuoteLine(s)
		for i := range len(q) {
			if q[i] < 0x20 || q[i] >= 0x7f {
				t.Errorf("QuoteLine(%q) = %q: byte %#x isn't printable ASCII", s, q, q[i])
				break
			}
		}
	}
	for _, bash := range bashes(t) {
		for _, loc := range locales(t) {
			for _, s := range words {
				cmd := exec.CommandContext(t.Context(), bash, "-c", "printf %s "+QuoteLine(s)) //nolint:gosec // quoting is what's tested
				cmd.Env = append(os.Environ(), "LC_ALL="+loc)
				got, err := cmd.Output()
				if err != nil || string(got) != s {
					t.Errorf("%s, %s: QuoteLine(%q) gave %q, %v", bash, loc, s, got, err)
				}
			}
			if _, err := os.Stat(ran); err == nil {
				t.Fatalf("%s, %s: a quoted word ran a command", bash, loc)
			}
		}
	}
}

// A NUL can't be in a bash string: QuoteLine panics rather than drop it.
func TestQuoteLineNUL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("QuoteLine with a NUL didn't panic")
		}
	}()
	QuoteLine("a\x00b")
}

func FuzzQuote(f *testing.F) {
	for _, s := range nasty {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			return // no argument holds one
		}
		if out, err := sh(t, "printf %s "+Quote(s)); err != nil || out != s {
			t.Errorf("Quote(%q): the shell got %q, %v", s, out, err)
		}
	})
}

func TestEnvName(t *testing.T) {
	for _, ok := range []string{"A", "_", "HOME", "http_proxy", "_X1", "a9_b"} {
		if !EnvName(ok) {
			t.Errorf("EnvName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "1X", "9", "A-B", "A B", "A=B", "É", "a.b", "$A", "A\n", "-"} {
		if EnvName(bad) {
			t.Errorf("EnvName(%q) = true", bad)
		}
	}
}
