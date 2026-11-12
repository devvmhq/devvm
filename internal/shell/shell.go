// Package shell is how devvm writes for a POSIX shell: a word quoted, a
// command line of quoted words, and the names a shell's variables can
// have; and for bash, a word quoted on one line. Standard library only, so every package can use it.
package shell

import "strings"

// Quote quotes s as one word for a POSIX shell: in single quotes, each
// single quote in s ending the quotes, then a backslash-escaped quote,
// then opening them again. Nothing inside single quotes is special, so
// any bytes come through as they are.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteLine quotes s as one word on one line for bash, not a POSIX sh:
// ANSI-C quoting, $'…', which bash 3.2 reads back as s. A backslash, a
// single quote and a newline are escaped, and every other byte that isn't
// printable ASCII is written as \xHH, so the word is printable ASCII
// alone: in a locale such as Shift-JIS or GBK, a raw byte from 0x80 up
// could start a character that swallows the backslash escaping a quote.
// A bash string can't hold a NUL, so a NUL in s is a caller's bug, and
// QuoteLine panics rather than drop it.
func QuoteLine(s string) string {
	var b strings.Builder
	b.WriteString("$'")
	for i := range len(s) {
		switch c := s[i]; {
		case c == 0:
			panic("shell.QuoteLine: a NUL, which a bash string can't hold")
		case c == '\\' || c == '\'':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c < 0x20 || c >= 0x7f:
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("'")
	return b.String()
}

const hex = "0123456789abcdef"

// QuoteJoin quotes each word and joins them with spaces: a command line.
func QuoteJoin(words []string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = Quote(w)
	}
	return strings.Join(q, " ")
}

// EnvName reports whether s can name a shell's variable: a letter or an
// underscore, then letters, digits and underscores.
func EnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
