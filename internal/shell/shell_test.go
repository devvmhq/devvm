package shell

import (
	"os/exec"
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

// Word leaves a plain path as it is and quotes the rest, which reaches
// the shell as it was.
func TestWord(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/libexec/devvm/devvm-immutable": "/usr/local/libexec/devvm/devvm-immutable",
		"/home/a/.local/lib/devvm/0.2.0-rc.1/x+y":  "/home/a/.local/lib/devvm/0.2.0-rc.1/x+y",
		"/home/a b/x": `'/home/a b/x'`,
		"":            `''`,
		"~/x":         `'~/x'`,
	} {
		if got := Word(in); got != want {
			t.Errorf("Word(%q) = %s, want %s", in, got, want)
		}
	}
	for _, s := range nasty {
		if out, err := sh(t, "printf %s "+Word(s)); err != nil || out != s {
			t.Errorf("Word(%q): the shell got %q, %v", s, out, err)
		}
	}
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
