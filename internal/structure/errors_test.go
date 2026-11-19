package structure

import (
	"go/ast"
	"maps"
	"slices"
	"strings"
)

// ruleErrors is checkDroppedErrors' name, as its failures start.
const ruleErrors = "dropped-errors"

// strconvParse is whether fn is one of package strconv's functions whose
// last result is the error that says the text wasn't what was asked for.
func strconvParse(fn string) bool {
	switch fn {
	case "Atoi", "ParseBool", "ParseInt", "ParseUint", "ParseFloat", "ParseComplex", "Unquote", "UnquoteChar", "QuotedPrefix":
		return true
	}
	return false
}

// dropsError is what a call whose last result is assigned to _ drops, or
// "": a process's end (x.Wait()) or a strconv parse's error, which would
// otherwise read a failed wait as a clean exit or text that isn't a number
// as 0.
func dropsError(f srcFile, call *ast.CallExpr) string {
	if callsMethod(call, "Wait") && len(call.Args) == 0 {
		return "drops a Wait's error"
	}
	name := callName(call)
	if pkg := importName(f, "strconv"); pkg != "" {
		if fn, ok := strings.CutPrefix(name, pkg+"."); ok && strconvParse(fn) {
			return "drops " + name + "'s error"
		}
	}
	return ""
}

// blankLast is whether the last of lhs is _.
func blankLast(lhs []ast.Expr) bool {
	if len(lhs) == 0 {
		return false
	}
	id, ok := lhs[len(lhs)-1].(*ast.Ident)
	return ok && id.Name == "_"
}

// A drop is where a function first breaks a siteRule, and how.
type drop struct {
	at   ast.Node
	what string
}

// sitesWhere are f's functions, by siteName, with the first node in each
// that what says breaks a rule (what isn't "").
func sitesWhere(f srcFile, what func(ast.Node) string) map[string]drop {
	return sitesWhereIn(f, func(*ast.FuncDecl) func(ast.Node) string { return what })
}

// sitesWhereIn is sitesWhere with what made for each function from its
// declaration, for a rule that reads the function first (its locals).
func sitesWhereIn(f srcFile, whatIn func(*ast.FuncDecl) func(ast.Node) string) map[string]drop {
	sites := map[string]drop{}
	for _, d := range f.ast.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		site, what := siteName(f, fd), whatIn(fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if _, seen := sites[site]; !seen && n != nil {
				if w := what(n); w != "" {
					sites[site] = drop{n, w}
				}
			}
			return true
		})
	}
	return sites
}

// droppedErrors are f's functions, by siteName, that assign a Wait's or a
// strconv parse's error to _, each with its first drop.
func droppedErrors(f srcFile) map[string]drop {
	return sitesWhere(f, func(n ast.Node) string {
		var lhs, rhs []ast.Expr
		switch n := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = n.Lhs, n.Rhs
		case *ast.ValueSpec:
			for _, id := range n.Names {
				lhs = append(lhs, id)
			}
			rhs = n.Values
		}
		if len(rhs) != 1 || !blankLast(lhs) {
			return ""
		}
		if call, ok := rhs[0].(*ast.CallExpr); ok {
			return dropsError(f, call)
		}
		return ""
	})
}

// A siteRule holds calls of one kind outside tests to the functions its
// table lists, each with why: find finds a file's, by siteName; fix says
// what to do instead, and none what a listed function that has none
// lacks, which fails, so the table stays current.
type siteRule struct {
	rule, table string
	listed      map[string]string
	find        func(srcFile) map[string]drop
	fix, none   string
}

// check is s's violations in m.
func (s siteRule) check(m *module) []violation {
	var vs []violation
	seen := map[string]bool{}
	for _, f := range m.files {
		if f.test {
			continue
		}
		for site, d := range s.find(f) {
			seen[site] = true
			if _, ok := s.listed[site]; !ok {
				vs = append(vs, violation{s.rule, m.pos(d.at), d.what + ": " + s.fix + ", or list " + site + " in " + s.table + " with why"})
			}
		}
	}
	for _, site := range slices.Sorted(maps.Keys(s.listed)) {
		switch {
		case !seen[site]:
			vs = append(vs, violation{s.rule, s.table, site + " has " + s.none + ": take it out"})
		case strings.TrimSpace(s.listed[site]) == "":
			vs = append(vs, violation{s.rule, s.table, site + ": say why"})
		}
	}
	return vs
}

// checkDroppedErrors: outside tests, no function assigns a Wait's error,
// or a strconv parse's, to _, but one that droppedErrors lists with why; a
// listed function that drops none fails. The check reads the source
// alone, so any method named Wait counts.
func checkDroppedErrors(m *module, r rules) []violation {
	return siteRule{ruleErrors, "droppedErrors", r.droppedErrors, droppedErrors, "handle it", "no dropped error"}.check(m)
}

// errorCases break and keep dropped-errors.
func errorCases() []ruleCase {
	file := func(body string) map[string]string {
		return map[string]string{"a/a.go": "package a\n\nimport (\n\t\"os/exec\"\n\tconv \"strconv\"\n)\n\n" +
			"var _ = conv.Itoa\n\nvar _ *exec.Cmd\n\n" + body}
	}
	return []ruleCase{
		{
			name:  "a Wait's error assigned to _",
			files: file("func run(c *exec.Cmd) { _ = c.Wait() }\n"),
			want:  &want{ruleErrors, "list a/a.go: run in droppedErrors"},
		},
		{
			name:  "a process's Wait in a goroutine, both results to _",
			files: file("type T struct{ c *exec.Cmd }\n\nfunc (t *T) stop() { go func() { _, _ = t.c.Process.Wait() }() }\n"),
			want:  &want{ruleErrors, "list a/a.go: T.stop in droppedErrors"},
		},
		{
			name:  "a strconv parse's error to _, its import renamed",
			files: file("func size(s string) int { n, _ := conv.Atoi(s); return n }\n"),
			want:  &want{ruleErrors, "list a/a.go: size in droppedErrors"},
		},
		{
			name:  "a strconv parse's error to _ in a var",
			files: file("func on(s string) bool { var b, _ = conv.ParseBool(s); return b }\n"),
			want:  &want{ruleErrors, "list a/a.go: on in droppedErrors"},
		},
		{
			name:  "a strconv parse's error to _ in an if",
			files: file("func size(s string) bool { if n, _ := conv.ParseInt(s, 10, 64); n > 0 { return true }; return false }\n"),
			want:  &want{ruleErrors, "list a/a.go: size in droppedErrors"},
		},
		{
			name: "errors handled, and a value to _",
			files: file("func run(c *exec.Cmd) error { return c.Wait() }\n\n" +
				"func size(s string) (int, error) { n, err := conv.Atoi(s); _ = conv.Itoa(n); return n, err }\n"),
		},
		{
			name:  "a listed drop",
			files: file("func run(c *exec.Cmd) { _ = c.Wait() }\n"),
			r:     rules{droppedErrors: map[string]string{"a/a.go: run": "its end is read from its output"}},
		},
		{
			name:  "a listed drop without why",
			files: file("func run(c *exec.Cmd) { _ = c.Wait() }\n"),
			r:     rules{droppedErrors: map[string]string{"a/a.go: run": " "}},
			want:  &want{ruleErrors, "a/a.go: run: say why"},
		},
		{
			name:  "a listed function dropping none",
			files: file("func run(c *exec.Cmd) error { return c.Wait() }\n"),
			r:     rules{droppedErrors: map[string]string{"a/a.go: run": "its end is read from its output"}},
			want:  &want{ruleErrors, "a/a.go: run has no dropped error: take it out"},
		},
		{
			name:  "a drop in a test",
			files: map[string]string{"a/a_test.go": "package a\n\nimport \"os/exec\"\n\nfunc run(c *exec.Cmd) { _ = c.Wait() }\n"},
		},
	}
}
