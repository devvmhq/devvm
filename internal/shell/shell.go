// Package shell is how devvm writes for a POSIX shell: a word quoted, a
// command line of quoted words, and the names a shell's variables can
// have. Standard library only, so every package can use it.
package shell

import "strings"

// Quote quotes s as one word for a POSIX shell: in single quotes, each
// single quote in s ending the quotes, then a backslash-escaped quote,
// then opening them again. Nothing inside single quotes is special, so
// any bytes come through as they are.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Word is s as one word for a POSIX shell, for a command a person reads:
// as it is when it has only letters, digits and / . _ + - :, which no
// shell treats specially, and quoted (Quote) otherwise.
func Word(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("/._+-:", r):
		default:
			return Quote(s)
		}
	}
	if s == "" {
		return Quote(s)
	}
	return s
}

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
