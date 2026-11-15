package structure

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"path"
	"slices"
	"strings"
)

// A hostRule is where a host is read in one form (checkHosts): the file that owns the
// normalization, and the functions outside it that may drop a dot or fold a case of their own,
// each with why.
type hostRule struct {
	owner string            // the file, from the module's root, that normalizes hosts
	free  map[string]string // "file:function", the file from the module's root: why it may
}

// hostish reports whether an expression names a host: an identifier, a field or a method whose
// name holds "host" in any case (host, h.Host, u.Hostname()), or is sni.
func hostish(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			l := strings.ToLower(id.Name)
			found = found || strings.Contains(l, "host") || l == "sni"
		}
		return !found
	})
	return found
}

// checkHosts: outside tests and r.hosts.owner, nothing reads a host in a form of its own. These
// calls to package strings fail, outside the functions free lists ("file:function"):
//   - Trim, TrimLeft or TrimRight with a cutset that holds ".", and TrimSuffix or CutSuffix with
//     ".", which drop a trailing dot;
//   - ToLower, ToUpper, EqualFold, TrimFunc, TrimLeftFunc or TrimRightFunc of an argument that
//     names a host (host, sni, u.Hostname()), which fold its case or trim it a way of their own.
//
// So does a conversion to the owner's Host of anything but a literal: proxy.NormHost reads a host,
// as the proxy does.
//
// It goes by names and by calls, so it doesn't see a host's case folded in a variable named
// otherwise (name, s), a dot dropped by slicing (h[:len(h)-1]), or a host mapped rune by rune
// (strings.Map, unicode.ToLower).
func checkHosts(m *module, r rules) []violation {
	o := r.hosts
	if o.owner == "" {
		return nil
	}
	ownerDir := path.Dir(o.owner)
	var vs []violation
	used := map[string]bool{}
	c := stringConsts(m)
	for _, f := range m.files {
		if f.test || f.name == o.owner {
			continue
		}
		pkg := importName(f, "strings")
		host := importName(f, m.path+"/"+ownerDir) + ".Host"
		if f.dir == ownerDir {
			host = "Host"
		}
		for _, d := range f.ast.Decls {
			site := f.name + ":" + declName(d)
			ast.Inspect(d, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isConversion(call, host) {
					if lit, ok := call.Args[0].(*ast.BasicLit); !ok || lit.Kind != token.STRING {
						vs = append(vs, violation{ruleOwners, m.pos(call), "makes a Host of a string: read it with NormHost (" + o.owner + ")"})
					}
					return true
				}
				why := hostCall(m, f, c, pkg, call)
				switch {
				case why == "":
				case o.free[site] != "":
					used[site] = true
				default:
					vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("%s: read the host with proxy.NormHost (%s)", why, o.owner)})
				}
				return true
			})
		}
	}
	for _, site := range slices.Sorted(maps.Keys(o.free)) {
		if !used[site] {
			vs = append(vs, violation{ruleOwners, "hosts", site + " reads no host of its own: take it out"})
		}
	}
	return vs
}

// hostCall is why call, in f, reads a host a way of its own (checkHosts), or "".
func hostCall(m *module, f srcFile, c consts, pkg string, call *ast.CallExpr) string {
	if pkg == "" || len(call.Args) == 0 {
		return ""
	}
	last := call.Args[len(call.Args)-1]
	switch callName(call) {
	case pkg + ".Trim", pkg + ".TrimLeft", pkg + ".TrimRight":
		if v, ok := c.value(m, f, last); ok && strings.Contains(v, ".") {
			return "drops a dot"
		}
	case pkg + ".TrimSuffix", pkg + ".CutSuffix":
		if v, ok := c.value(m, f, last); ok && v == "." {
			return "drops a trailing dot"
		}
	case pkg + ".TrimFunc", pkg + ".TrimLeftFunc", pkg + ".TrimRightFunc":
		if hostish(call.Args[0]) {
			return "trims a host"
		}
	case pkg + ".ToLower", pkg + ".ToUpper", pkg + ".EqualFold":
		if slices.ContainsFunc(call.Args, hostish) {
			return "folds a host's case"
		}
	}
	return ""
}

// declName is a declaration's function's name, Type.Name for a method, or "" for any other.
func declName(d ast.Decl) string {
	fd, ok := d.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// isConversion reports whether call is name(x), one argument, name an identifier or pkg.Name.
func isConversion(call *ast.CallExpr, name string) bool {
	if len(call.Args) != 1 {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == name
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		return ok && x.Name+"."+fun.Sel.Name == name
	}
	return false
}
