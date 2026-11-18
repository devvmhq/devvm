package structure

import (
	"go/ast"
	"go/token"
)

// ruleFolderIDs is checkFolderIDs' name, as its failures start.
const ruleFolderIDs = "folder-ids"

// folderIDOperand is whether e reads like a folder's identity
// (platform.FolderID): a field named Folder or GitID, as a VM's state and
// .git protection's record hold them, or a call of FolderID or folderID.
// The check reads no types, so any field by those names counts.
func folderIDOperand(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "Folder" || e.Sel.Name == "GitID"
	case *ast.CallExpr:
		switch fun := e.Fun.(type) {
		case *ast.SelectorExpr:
			return fun.Sel.Name == "FolderID"
		case *ast.Ident:
			return fun.Name == "FolderID" || fun.Name == "folderID"
		}
	}
	return false
}

// emptyString is whether e is the literal "".
func emptyString(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

// comparesFolderIDs are f's functions, by siteName, that compare a
// folder's identity with == or != (folderIDOperand), but with "", each
// with its first such comparison; none in owner, the package folder that
// compares them.
func comparesFolderIDs(owner string) func(srcFile) map[string]drop {
	return func(f srcFile) map[string]drop {
		if f.dir == owner {
			return nil
		}
		return sitesWhere(f, func(n ast.Node) string {
			b, ok := n.(*ast.BinaryExpr)
			if !ok || b.Op != token.EQL && b.Op != token.NEQ || emptyString(b.X) || emptyString(b.Y) {
				return ""
			}
			if folderIDOperand(b.X) || folderIDOperand(b.Y) {
				return "compares a folder's identity with " + b.Op.String()
			}
			return ""
		})
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
