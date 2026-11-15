package jsonc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestStrip(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"line comment", "{\"a\": 1 // c\n}", "{\"a\": 1     \n}"},
		{"block comment", "{/* x\ny */\"a\": 1}", "{    \n    \"a\": 1}"},
		{"trailing commas", `{"a": [1, 2,], "b": {"c": 3,},}`, `{"a": [1, 2 ], "b": {"c": 3 } }`},
		{"comma before a comment", "{\"a\": 1, // c\n}", "{\"a\": 1      \n}"},
		{"slashes in strings", `{"a": "http://x/*y*/", "b": "q\"//"}`, `{"a": "http://x/*y*/", "b": "q\"//"}`},
		{"comma in a string", `{"a": ",}"}`, `{"a": ",}"}`},
		{"escaped backslash", `{"a": "\\", "b": 1,}`, `{"a": "\\", "b": 1 }`},
		{"bom", "\xef\xbb\xbf{}", "   {}"},
		{"unterminated block", "{} /* x", "{}     "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(Strip([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) != len(tc.in) {
				t.Errorf("length changed: %d, want %d", len(got), len(tc.in))
			}
		})
	}
}

// withComments is the JSON in, indented a value a line, with a comment after each line marks
// says (a line's, or a block's) and a trailing comma before each } or ] that ends an object or a
// list, and a byte order mark first with bom.
func withComments(in []byte, comment string, marks []byte, bom bool) []byte {
	var b bytes.Buffer
	if json.Indent(&b, in, "", "  ") != nil {
		return nil
	}
	lines := strings.Split(b.String(), "\n")
	var out strings.Builder
	if bom {
		out.WriteString("\xef\xbb\xbf")
	}
	for i, l := range lines {
		out.WriteString(l)
		if i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if l != "" && !strings.HasSuffix(l, "{") && !strings.HasSuffix(l, "[") && (strings.HasPrefix(next, "}") || strings.HasPrefix(next, "]")) {
				out.WriteString(",")
			}
		}
		var m byte
		if i < len(marks) {
			m = marks[i]
		}
		switch {
		case m%3 == 1 && !strings.ContainsAny(comment, "\r\n"):
			out.WriteString(" //" + comment)
		case m%3 == 2 && !strings.Contains(comment, "*/"):
			out.WriteString(" /*" + comment + "*/")
		}
		if i+1 < len(lines) {
			out.WriteString("\n")
		}
	}
	return []byte(out.String())
}

// decoded is b as encoding/json reads it, its numbers as written.
func decoded(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}

// FuzzStrip: Strip keeps every offset, blanking bytes with spaces but newlines; leaves JSON as it
// is; and, against encoding/json, makes JSON with comments, trailing commas and a byte order mark
// the value the JSON without them is.
func FuzzStrip(f *testing.F) {
	for _, s := range []string{`{"a": [1, 2], "b": {"c": "//x/*y*/\""}}`, `[]`, `"s"`, `{"a":{"b":[{"c":null}]}}`, "{} /* x", `{"a":1,}`} {
		f.Add([]byte(s), "c", []byte{1, 2, 0, 1}, false)
		f.Add([]byte(s), "a\"b*", []byte{2, 2, 2}, true)
	}
	f.Fuzz(func(t *testing.T, data []byte, comment string, marks []byte, bom bool) {
		got := Strip(data)
		if len(got) != len(data) {
			t.Fatalf("Strip(%q) = %q: another length", data, got)
		}
		for i := range data {
			if got[i] != data[i] && (got[i] != ' ' || data[i] == '\n' || data[i] == '\r') {
				t.Fatalf("Strip(%q) = %q: byte %d", data, got, i)
			}
		}
		if !json.Valid(data) {
			return
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("Strip(%q) = %q: JSON changed", data, got)
		}
		in := withComments(data, comment, marks, bom)
		want, err := decoded(data)
		if err != nil || in == nil {
			t.Fatalf("%q: %v", data, err)
		}
		v, err := decoded(Strip(in))
		if err != nil || !reflect.DeepEqual(v, want) {
			t.Fatalf("Strip(%q) reads as %v, %v; want %v", in, v, err, want)
		}
	})
}
