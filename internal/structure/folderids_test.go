package structure

import (
	"go/ast"
	"go/token"
	"slices"
)

// ruleFolderIDs is checkFolderIDs' name, as its failures start.
const ruleFolderIDs = "folder-ids"

// folderIDOperand is whether e reads like a folder's identity
// (platform.FolderID) or its volume: a field named Folder or GitID, as .git
// protection's record holds them, a call of a VM state's Folder or
// FolderVolume, or of FolderID, folderID or FolderDevice, or a name in
// locals, a function's own names that one of those was assigned to. The
// check reads no types, so any field or method by those names counts.
func folderIDOperand(e ast.Expr, locals map[string]bool) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return locals[e.Name]
	case *ast.SelectorExpr:
		return e.Sel.Name == "Folder" || e.Sel.Name == "GitID"
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.SelectorExpr:
			return slices.Contains([]string{"Folder", "FolderVolume", "FolderID", "FolderDevice"}, fun.Sel.Name)
		case *ast.Ident:
			return fun.Name == "FolderID" || fun.Name == "folderID"
		}
	}
	return false
}

// folderIDLocals are the names in body that a folder's identity is assigned
// to (folderIDOperand), by := or =, or declared with, so a comparison of
// one, which reads like any string's, is still found.
func folderIDLocals(body *ast.BlockStmt) map[string]bool {
	locals := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		var lhs, rhs []ast.Expr
		switch n := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = n.Lhs, n.Rhs
		case *ast.ValueSpec:
			for _, id := range n.Names {
				lhs = append(lhs, id)
			}
			rhs = n.Values
		default:
			return true
		}
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			switch {
			case !ok:
			case len(lhs) == len(rhs) && folderIDOperand(rhs[i], nil),
				// id, err := FolderID(dir): the call's first result.
				i == 0 && len(rhs) == 1 && folderIDOperand(rhs[0], nil):
				locals[id.Name] = true
			}
		}
		return true
	})
	return locals
}

// comparesOne is how n compares a folder's identity (folderIDOperand, with
// locals), but with "": == or !=, a switch on one, or strings.EqualFold,
// strings.Compare, slices.Contains or slices.Index with one; "" for none.
func comparesOne(n ast.Node, locals map[string]bool) string {
	is := func(e ast.Expr) bool { return folderIDOperand(e, locals) }
	switch n := n.(type) {
	case *ast.BinaryExpr:
		if n.Op != token.EQL && n.Op != token.NEQ || emptyString(n.X) || emptyString(n.Y) {
			return ""
		}
		if is(n.X) || is(n.Y) {
			return "compares a folder's identity with " + n.Op.String()
		}
	case *ast.SwitchStmt:
		if n.Tag != nil && is(n.Tag) {
			return "switches on a folder's identity"
		}
	case *ast.CallExpr:
		sel, ok := n.Fun.(*ast.SelectorExpr)
		cmp := false
		if ok {
			x, xok := sel.X.(*ast.Ident)
			cmp = xok && (x.Name == "strings" && (sel.Sel.Name == "EqualFold" || sel.Sel.Name == "Compare") ||
				x.Name == "slices" && (sel.Sel.Name == "Contains" || sel.Sel.Name == "Index"))
		}
		if cmp && slices.ContainsFunc(n.Args, is) {
			return "compares a folder's identity with " + sel.Sel.Name
		}
	}
	return ""
}

// emptyString is whether e is the literal "".
func emptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// comparesFolderIDs are f's functions, by siteName, that compare a
// folder's identity (comparesOne), each with its first such comparison;
// none in owner, the package folder that compares them.
func comparesFolderIDs(owner string) func(srcFile) map[string]drop {
	return func(f srcFile) map[string]drop {
		if f.dir == owner {
			return nil
		}
		sites := map[string]drop{}
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			site, locals := siteName(f, fd), folderIDLocals(fd.Body)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if _, seen := sites[site]; !seen && n != nil {
					if w := comparesOne(n, locals); w != "" {
						sites[site] = drop{n, w}
					}
				}
				return true
			})
		}
		return sites
	}
}

// checkFolderIDs: outside tests and the owner (folderIDOwner), no
// function compares a folder's identity with == or !=, as a boot can
// number an unchanged folder's device anew (platform.SameFolder), unless
// exactFolderIDs lists it with why; a listed function that has none
// fails. An empty owner checks none.
func checkFolderIDs(m *module, r rules) []violation {
	if r.folderIDOwner == "" {
		return nil
	}
	return siteRule{ruleFolderIDs, "exactFolderIDs", r.exactFolderIDs, comparesFolderIDs(r.folderIDOwner), "compare with platform.SameFolder", "no comparison of a folder's identity"}.check(m)
}

// folderIDCases break and keep folder-ids.
func folderIDCases() []ruleCase {
	file := func(body string) map[string]string {
		return map[string]string{"a/a.go": "package a\n\nimport \"example.com/m/p\"\n\n" +
			"type VM struct{ Dir, Folder string }\n\ntype Repo struct{ GitID string }\n\n" + body}
	}
	owner := rules{folderIDOwner: "p"}
	withExact := func(listed map[string]string) rules { return rules{folderIDOwner: "p", exactFolderIDs: listed} }
	return []ruleCase{
		{
			name:  "a VM's folder compared with ==",
			files: file("func same(st VM, dir string) bool { id, _ := p.FolderID(dir); return st.Folder == id }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: same in exactFolderIDs"},
		},
		{
			name:  "a .git's identity compared with != in a method",
			files: file("func (r Repo) moved(id string) bool { return id != r.GitID }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: Repo.moved in exactFolderIDs"},
		},
		{
			name:  "a call of FolderID compared",
			files: file("func here(dir, was string) bool { return p.FolderID(dir) == was }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: here in exactFolderIDs"},
		},
		{
			name:  "a state's Folder() compared",
			files: file("type S struct{}\n\nfunc (S) Folder() string { return \"\" }\n\nfunc moved(st S, id string) bool { return st.Folder() != id }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: moved in exactFolderIDs"},
		},
		{
			name:  "a state's FolderVolume() compared",
			files: file("type S struct{}\n\nfunc (S) FolderVolume() string { return \"\" }\n\nfunc moved(st S, v string) bool { return v == st.FolderVolume() }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: moved in exactFolderIDs"},
		},
		{
			name:  "a local an identity was assigned to, compared",
			files: file("func moved(dir, want string) bool { id := p.FolderID(dir); return id != want }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: moved in exactFolderIDs"},
		},
		{
			name:  "a switch on a device",
			files: file("func dev(id string) bool { switch p.FolderDevice(id) { case \"1\": return true }; return false }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: dev in exactFolderIDs"},
		},
		{
			name:  "strings.EqualFold on one",
			files: file("func same(st VM, id string) bool { return strings.EqualFold(st.Folder, id) }\n"),
			r:     owner,
			want:  &want{ruleFolderIDs, "list a/a.go: same in exactFolderIDs"},
		},
		{
			name:  "compared with the owner's",
			files: file("func same(st VM, dir, id string) bool { return p.SameFolder(st.Dir, st.Folder, dir, id) }\n"),
			r:     owner,
		},
		{
			name:  "compared with \"\"",
			files: file("func none(st VM) bool { return st.Folder == \"\" }\n"),
			r:     owner,
		},
		{
			name:  "in the owner",
			files: map[string]string{"p/p.go": "package p\n\nfunc FolderID(string) string { return \"\" }\n\nfunc Same(was, dir string) bool { return FolderID(dir) == was }\n"},
			r:     owner,
		},
		{
			name:  "a listed comparison",
			files: file("func same(st VM, id string) bool { return st.Folder == id }\n"),
			r:     withExact(map[string]string{"a/a.go: same": "the record unchanged since it was read"}),
		},
		{
			name:  "a listed function comparing none",
			files: file("func same(st VM, id string) bool { return p.SameFolder(st.Dir, st.Folder, st.Dir, id) }\n"),
			r:     withExact(map[string]string{"a/a.go: same": "the record unchanged since it was read"}),
			want:  &want{ruleFolderIDs, "a/a.go: same has no comparison of a folder's identity: take it out"},
		},
		{
			name:  "a comparison in a test",
			files: map[string]string{"a/a_test.go": "package a\n\ntype VM struct{ Folder string }\n\nfunc same(st VM, id string) bool { return st.Folder == id }\n"},
			r:     owner,
		},
	}
}
