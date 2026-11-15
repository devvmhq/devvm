package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A Secret prints as Hidden whatever the verb, in a struct, a pointer or a slice, and encodes as
// nothing but ErrEncode, so the value never reaches a log, an error or a file by accident.
func TestSecret(t *testing.T) {
	t.Parallel()
	const value = "e2e-not-a-secret-value"
	s := NewSecret(value)
	if s.Reveal() != value || s.Empty() || !(Secret{}).Empty() {
		t.Fatalf("Reveal %q, Empty %v", s.Reveal(), s.Empty())
	}
	type holder struct {
		Name string
		S    Secret
		P    *Secret
	}
	h := holder{"n", s, &s}
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10s", "%T"} {
		for _, v := range []any{s, &s, h, &h, []Secret{s}, map[string]Secret{"k": s}} {
			got := fmt.Sprintf(f, v)
			if strings.Contains(got, value) || strings.Contains(got, fmt.Sprintf("%x", value)) {
				t.Errorf("Sprintf(%q, %T) = %q: the value shows", f, v, got)
			}
		}
	}
	// fmt prints an unexported field raw, never calling its Format: it shows the pointer alone.
	type private struct {
		name string
		s    Secret
	}
	type nested struct {
		p   private
		ptr *private
	}
	pv := private{"n", s}
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{pv, &pv, nested{pv, &pv}, []private{pv}, map[string]private{"k": pv}} {
			got := fmt.Sprintf(f, v)
			if strings.Contains(got, value) || strings.Contains(got, fmt.Sprintf("%x", value)) {
				t.Errorf("Sprintf(%q, %T) = %q: the value shows", f, v, got)
			}
		}
	}
	if NewSecret("") != (Secret{}) || !NewSecret(" x ").TrimSpace().Empty() && NewSecret(" x ").TrimSpace().Reveal() != "x" ||
		!NewSecret(" ").TrimSpace().Empty() || (Secret{}).Reveal() != "" || (Secret{}).TrimSpace() != (Secret{}) {
		t.Error("NewSecret, TrimSpace or Reveal of the zero Secret")
	}
	if got := fmt.Sprint(s) + s.String() + s.GoString(); got != Hidden+Hidden+Hidden {
		t.Errorf("printed as %q", got)
	}
	if err := fmt.Errorf("lookup: %v", s); strings.Contains(err.Error(), value) {
		t.Errorf("error %q", err)
	}
	for _, v := range []any{s, &s, h, []Secret{s}} {
		if b, err := json.Marshal(v); !errors.Is(err, ErrEncode) {
			t.Errorf("json.Marshal(%T) = %s, %v, want ErrEncode", v, b, err)
		}
	}
	if b, err := json.Marshal(map[Secret]int{s: 1}); err == nil {
		t.Errorf("a map keyed by a secret encodes: %s", b)
	}
	if b, err := s.MarshalText(); !errors.Is(err, ErrEncode) {
		t.Errorf("MarshalText = %s, %v", b, err)
	}
	var back holder
	if err := json.Unmarshal([]byte(`{"S":"x"}`), &back); !errors.Is(err, ErrEncode) || !back.S.Empty() {
		t.Errorf("json.Unmarshal = %v, %v, want ErrEncode", back.S, err)
	}
}
