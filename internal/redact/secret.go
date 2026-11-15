package redact

import (
	"errors"
	"fmt"
	"strings"
)

// Secret is a value that lets whoever holds it act as the user or as a VM: a VM's proxy password,
// a token, a login's access or refresh token, the company proxy's password. Printed in any way
// (String, GoString, every verb of fmt), it's Hidden, so a log line, an error or a %v of a struct
// that holds one never shows it. Encoding it is an error, never a stand-in (MarshalJSON,
// MarshalText), so a file that holds the real value writes it through its own wire type, which
// calls Reveal, and a struct that holds one can't lose the value by being written somewhere it
// isn't meant to go. Reveal is the one way to the value, at call sites that each say why, in a
// directive to forbidigo. The zero Secret is empty.
//
// The value is behind a pointer: fmt can't call Format on a Secret in an unexported field, and
// prints such a field raw, which then shows only an address. Two Secrets are == only when they're
// the same one; compare values with Reveal, or structs that hold them with reflect.DeepEqual.
type Secret struct{ p *string }

// NewSecret is v as a Secret: the zero Secret for "".
func NewSecret(v string) Secret {
	if v == "" {
		return Secret{}
	}
	return Secret{&v}
}

// Hidden is what a Secret prints as, whatever the verb.
const Hidden = "<redacted>"

// ErrEncode is the error of encoding a Secret as JSON or text: a file that holds the value has a
// wire type of its own, which writes Reveal's string.
var ErrEncode = errors.New("a secret is never encoded: write it through its file's own type")

// Reveal is the value itself, for what must use it: a header, a file only the user can read, the
// Keychain. forbidigo refuses it except where a nolint says why.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// Empty reports whether the value is "".
func (s Secret) Empty() bool { return s.p == nil || *s.p == "" }

// TrimSpace is s without the white space around its value.
func (s Secret) TrimSpace() Secret {
	if s.p == nil {
		return s
	}
	return NewSecret(strings.TrimSpace(*s.p))
}

// String is Hidden.
func (Secret) String() string { return Hidden }

// GoString is Hidden, for %#v.
func (Secret) GoString() string { return Hidden }

// Format writes Hidden for every verb, so neither %x nor %q nor %+v shows the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Hidden)) }

// MarshalJSON is ErrEncode: the value never goes into JSON by accident, and a stand-in would lose
// it.
func (Secret) MarshalJSON() ([]byte, error) { return nil, ErrEncode }

// UnmarshalJSON is ErrEncode: a file that holds the value is read through its own wire type.
func (*Secret) UnmarshalJSON([]byte) error { return ErrEncode }

// MarshalText is ErrEncode, for the encoders that take text (TOML, a JSON map's keys).
func (Secret) MarshalText() ([]byte, error) { return nil, ErrEncode }
