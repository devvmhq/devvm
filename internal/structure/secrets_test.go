package structure

import (
	"go/ast"
	"path"
)

// checkSecrets: outside r.secretOwner, tests included, no struct embeds
// the owner's Secret, whose methods (String, Format, Reveal) the struct
// would then have as its own, so the struct itself would print as
// <redacted> and hand out the value through a Reveal nobody wrote; and no
// interface declares Reveal() string, which would let a value go through
// it with no call to Secret's Reveal for forbidigo to see. The check reads
// the source alone: a Secret embedded through another type, or reached
// through reflection, isn't seen.
func checkSecrets(m *module, r rules) []violation {
	if r.secretOwner == "" {
		return nil
	}
	pkg := m.path + "/" + r.secretOwner
	var vs []violation
	for _, f := range m.files {
		if f.dir == r.secretOwner {
			continue
		}
		name := importName(f, pkg)
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.StructType:
				for _, fl := range x.Fields.List {
					if len(fl.Names) == 0 && name != "" && isSecret(fl.Type, name) {
						vs = append(vs, violation{ruleOwners, m.pos(fl), "a struct embeds " + path.Base(r.secretOwner) + ".Secret: give it a field of its own"})
					}
				}
			case *ast.InterfaceType:
				for _, fl := range x.Methods.List {
					if ft, ok := fl.Type.(*ast.FuncType); ok && len(fl.Names) == 1 && fl.Names[0].Name == "Reveal" && revealsString(ft) {
						vs = append(vs, violation{ruleOwners, m.pos(fl), "an interface declares Reveal() string: take a " + path.Base(r.secretOwner) + ".Secret"})
					}
				}
			}
			return true
		})
	}
	return vs
}

// secretCases: checkSecrets' failures and what it leaves alone.
func secretCases() []ruleCase {
	r := rules{secretOwner: "internal/redact"}
	redact := "package redact\n\ntype Secret struct{ p *string }\n\nfunc (s Secret) Reveal() string { return *s.p }\n\ntype revealer interface{ Reveal() string }\n"
	uses := func(body string) string {
		return "package x\n\nimport \"example.com/m/internal/redact\"\n\n" + body
	}
	return []ruleCase{
		{
			name:  "a struct embeds a Secret",
			files: map[string]string{"internal/redact/r.go": redact, "internal/x/a.go": uses("type T struct{ redact.Secret }\n")},
			r:     r, want: &want{ruleOwners, "a struct embeds redact.Secret"},
		},
		{
			name:  "a test's struct embeds a pointer to a Secret, its import renamed",
			files: map[string]string{"internal/redact/r.go": redact, "internal/x/a_test.go": "package x\n\nimport s \"example.com/m/internal/redact\"\n\nvar _ = struct{ *s.Secret }{}\n"},
			r:     r, want: &want{ruleOwners, "a struct embeds redact.Secret"},
		},
		{
			name:  "an interface declares Reveal() string",
			files: map[string]string{"internal/redact/r.go": redact, "internal/x/a.go": "package x\n\ntype R interface {\n\tReveal() string\n}\n"},
			r:     r, want: &want{ruleOwners, "an interface declares Reveal() string"},
		},
		{
			name: "a Secret in a field of its own, and another Reveal",
			files: map[string]string{
				"internal/redact/r.go": redact,
				"internal/x/a.go":      uses("type T struct{ s redact.Secret }\n\ntype R interface{ Reveal(n int) string }\n"),
			},
			r: r,
		},
	}
}

// isSecret reports whether e is name.Secret or *name.Secret.
func isSecret(e ast.Expr, name string) bool {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == name && sel.Sel.Name == "Secret"
}

// revealsString reports whether ft is func() string.
func revealsString(ft *ast.FuncType) bool {
	if ft.Params != nil && len(ft.Params.List) > 0 || ft.Results == nil || len(ft.Results.List) != 1 || len(ft.Results.List[0].Names) > 1 {
		return false
	}
	id, ok := ft.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "string"
}
