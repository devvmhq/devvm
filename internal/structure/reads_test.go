package structure

import "go/ast"

// ruleReads is checkReads' name, as its failures start.
const ruleReads = "read-limits"

// unboundedRead is whether n is an io.ReadAll whose reader isn't an
// io.LimitReader made in the call, as io.ReadAll(io.LimitReader(r, n)).
func unboundedRead(f srcFile, n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	pkg := importName(f, "io")
	if !ok || pkg == "" || callName(call) != pkg+".ReadAll" || len(call.Args) != 1 {
		return false
	}
	return callName(call.Args[0]) != pkg+".LimitReader"
}

// unboundedReads are f's functions, by siteName, that read with no limit,
// each with its first such read.
func unboundedReads(f srcFile) map[string]drop {
	return sitesWhere(f, func(n ast.Node) string {
		if unboundedRead(f, n) {
			return "reads all of a reader with no limit"
		}
		return ""
	})
}

// checkReads: outside tests, every io.ReadAll reads through an
// io.LimitReader, made in its call, but in a function that unboundedReads
// lists with why, so a peer or a file that sends without end can't fill
// the memory; a listed function that has none fails.
func checkReads(m *module, r rules) []violation {
	return siteRule{ruleReads, "unboundedReads", r.unboundedReads, unboundedReads, "read through io.LimitReader", "no unbounded read"}.check(m)
}

// readCases break and keep read-limits.
func readCases() []ruleCase {
	file := func(body string) map[string]string {
		return map[string]string{"a/a.go": "package a\n\nimport (\n\tstdio \"io\"\n)\n\n" + body}
	}
	return []ruleCase{
		{
			name:  "a read with no limit, its import renamed",
			files: file("func read(r stdio.Reader) ([]byte, error) { return stdio.ReadAll(r) }\n"),
			want:  &want{ruleReads, "list a/a.go: read in unboundedReads"},
		},
		{
			name:  "a read through a reader limited before the call",
			files: file("type T struct{}\n\nfunc (T) read(r stdio.Reader) { l := stdio.LimitReader(r, 9); _, _ = stdio.ReadAll(l) }\n"),
			want:  &want{ruleReads, "list a/a.go: T.read in unboundedReads"},
		},
		{
			name:  "a read in a function literal",
			files: file("func read(r stdio.Reader) { go func() { _, _ = stdio.ReadAll(r) }() }\n"),
			want:  &want{ruleReads, "list a/a.go: read in unboundedReads"},
		},
		{
			name:  "a read through io.LimitReader",
			files: file("func read(r stdio.Reader) ([]byte, error) { return stdio.ReadAll(stdio.LimitReader(r, 9)) }\n"),
		},
		{
			name:  "a listed read",
			files: file("func read(r stdio.Reader) ([]byte, error) { return stdio.ReadAll(r) }\n"),
			r:     rules{unboundedReads: map[string]string{"a/a.go: read": "a file of its own, at most its size"}},
		},
		{
			name:  "a listed function reading none",
			files: file("func read(r stdio.Reader) ([]byte, error) { return stdio.ReadAll(stdio.LimitReader(r, 9)) }\n"),
			r:     rules{unboundedReads: map[string]string{"a/a.go: read": "a file of its own, at most its size"}},
			want:  &want{ruleReads, "a/a.go: read has no unbounded read: take it out"},
		},
		{
			name:  "a read with no limit in a test",
			files: map[string]string{"a/a_test.go": "package a\n\nimport \"io\"\n\nfunc read(r io.Reader) ([]byte, error) { return io.ReadAll(r) }\n"},
		},
	}
}
