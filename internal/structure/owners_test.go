package structure

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// consts are a module's string constants, by "folder.Name".
type consts map[string]string

// stringConsts finds every string constant m declares with a literal.
func stringConsts(m *module) consts {
	c := consts{}
	for _, f := range m.files {
		for _, d := range f.ast.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				spec, ok := s.(*ast.ValueSpec)
				if !ok || len(spec.Values) != len(spec.Names) {
					continue
				}
				for i, name := range spec.Names {
					if v, ok := literal(spec.Values[i]); ok {
						c[f.dir+"."+name.Name] = v
					}
				}
			}
		}
	}
	return c
}

// literal is a string literal's value.
func literal(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// importName is the name f imports the package p as, or "" when it
// doesn't.
func importName(f srcFile, p string) string {
	for _, imp := range f.ast.Imports {
		if v, err := strconv.Unquote(imp.Path.Value); err != nil || v != p {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path.Base(p)
	}
	return ""
}

// value is a string expression's value in f: a literal, a constant of f's
// package, or one of a package of the module's that f imports.
func (c consts) value(m *module, f srcFile, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		v, ok := c[f.dir+"."+x.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		for _, imp := range f.ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			dir, inModule := strings.CutPrefix(p, m.path+"/")
			if err != nil || !inModule {
				continue
			}
			name := path.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == pkg.Name {
				v, ok := c[dir+"."+x.Sel.Name]
				return v, ok
			}
		}
		return "", false
	}
	return literal(e)
}

// calls calls fn with every call in f.
func calls(f srcFile, fn func(*ast.CallExpr)) {
	ast.Inspect(f.ast, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			fn(call)
		}
		return true
	})
}

// checkCommands: outside tests, a host command that commands lists runs
// only in its package. An exec.Command or exec.CommandContext naming it,
// as a literal or a constant, runs it.
func checkCommands(m *module, r rules) []violation {
	var vs []violation
	c := stringConsts(m)
	for _, f := range m.files {
		exec := importName(f, "os/exec")
		if f.test || exec == "" {
			continue
		}
		at := map[string]int{exec + ".Command": 0, exec + ".CommandContext": 1} // the command's argument
		calls(f, func(call *ast.CallExpr) {
			i, ok := at[callName(call)]
			if !ok || len(call.Args) <= i {
				return
			}
			name, ok := c.value(m, f, call.Args[i])
			if owner, guarded := r.commands[name]; ok && guarded && f.dir != owner {
				vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("runs %s: run it through %s, the only package that does", name, owner)})
			}
		})
	}
	return vs
}

// checkExecSites: outside tests, only the files execSites lists make a
// host command (exec.Command or exec.CommandContext); the rest run theirs
// through a runner. A listed file that no longer makes one fails.
func checkExecSites(m *module, r rules) []violation {
	if r.execSites == nil {
		return nil
	}
	var vs []violation
	made := map[string]bool{}
	for _, f := range m.files {
		exec := importName(f, "os/exec")
		if f.test || exec == "" {
			continue
		}
		calls(f, func(call *ast.CallExpr) {
			if name := callName(call); name != exec+".Command" && name != exec+".CommandContext" {
				return
			}
			made[f.name] = true
			if _, listed := r.execSites[f.name]; !listed {
				vs = append(vs, violation{ruleOwners, m.pos(call), "makes a host command: run it through the UI's LoggedCmd, the backend's Runner or hostgit, or list the file in execSites with why"})
			}
		})
	}
	for _, file := range slices.Sorted(maps.Keys(r.execSites)) {
		if !made[file] {
			vs = append(vs, violation{ruleOwners, "execSites", file + " makes no host command: take it out"})
		}
	}
	return vs
}

// checkGuard: outside tests and guardOwner, no type assertion or type
// switch names a type of internal/backend or a package below it: the
// registry's backends are behind its guard, whose type has Backend's
// methods alone, so an assertion to another interface of theirs is
// always false.
func checkGuard(m *module, r rules) []violation {
	if r.guardOwner == "" {
		return nil
	}
	backendTypes := map[string]bool{} // the types internal/backend declares
	for _, f := range m.files {
		if f.dir != "internal/backend" {
			continue
		}
		for _, d := range f.ast.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, s := range gd.Specs {
					backendTypes[s.(*ast.TypeSpec).Name.Name] = true
				}
			}
		}
	}
	var vs []violation
	for _, f := range m.files {
		if f.test || f.name == r.guardOwner {
			continue
		}
		check := func(e ast.Expr) {
			if name, ok := backendType(m, f, e, backendTypes); ok {
				vs = append(vs, violation{ruleOwners, m.pos(e), "asserts " + name + ": a registry's backend is behind its guard, so use backend.As"})
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeAssertExpr:
				if x.Type != nil {
					check(x.Type)
				}
			case *ast.TypeSwitchStmt:
				for _, c := range x.Body.List {
					for _, e := range c.(*ast.CaseClause).List {
						check(e)
					}
				}
			}
			return true
		})
	}
	return vs
}

// backendType reports whether e, a type in f, is one of internal/backend's
// or a package's below it, and names it.
func backendType(m *module, f srcFile, e ast.Expr, backendTypes map[string]bool) (string, bool) {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name, f.dir == "internal/backend" && backendTypes[x.Name]
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		for _, imp := range f.ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			dir, inModule := strings.CutPrefix(p, m.path+"/")
			if err != nil || !inModule || (dir != "internal/backend" && !strings.HasPrefix(dir, "internal/backend/")) {
				continue
			}
			name := path.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == pkg.Name {
				return pkg.Name + "." + x.Sel.Name, true
			}
		}
	}
	return "", false
}

// funcName is the name of the function a call calls: F for F(), x.F() and
// x.y.F().
func funcName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// checkEnv: outside tests, a DEVVM_ variable is read only in envOwner, or
// in the files envSettings lists for it, and a variable envOwners lists
// only in its file. A call to a function or a field
// named Getenv or LookupEnv, in any case, reads its argument, a literal
// or a constant. A listed file that no longer reads its variable fails.
func checkEnv(m *module, r rules) []violation {
	var vs []violation
	c := stringConsts(m)
	read := map[string]bool{} // "NAME file"
	for _, f := range m.files {
		if f.test {
			continue
		}
		calls(f, func(call *ast.CallExpr) {
			fn := strings.ToLower(funcName(call))
			if (fn != "getenv" && fn != "lookupenv") || len(call.Args) != 1 {
				return
			}
			name, ok := c.value(m, f, call.Args[0])
			if owner, owned := r.envOwners[name]; ok && owned {
				if f.name != owner {
					vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("reads %s: only %s reads it; take it from there", name, owner)})
				}
				return
			}
			if !ok || !strings.HasPrefix(name, "DEVVM_") || f.name == r.envOwner {
				return
			}
			if s, listed := r.envSettings[name]; listed && slices.Contains(s.files, f.name) {
				read[name+" "+f.name] = true
				return
			}
			vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("reads %s: a test build's stand-in is read in %s, a setting is listed in envSettings with why", name, r.envOwner)})
		})
	}
	for name, s := range r.envSettings {
		for _, file := range s.files {
			if !read[name+" "+file] {
				vs = append(vs, violation{ruleOwners, "envSettings", name + " isn't read in " + file + ": take it out"})
			}
		}
	}
	return vs
}

// checkLiterals: outside tests, a string literal holding a text that
// literals lists is in its owner's package: one anywhere else is a copy
// of what that package owns.
func checkLiterals(m *module, r rules) []violation {
	var vs []violation
	for _, f := range m.files {
		if f.test {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, ok := literal(lit)
			if !ok {
				return true
			}
			for _, text := range slices.Sorted(maps.Keys(r.literals)) {
				if o := r.literals[text]; strings.Contains(v, text) && f.dir != o.dir {
					vs = append(vs, violation{ruleOwners, m.pos(lit), fmt.Sprintf("holds %s: a copy of what %s owns, so use %s", text, o.dir, o.use)})
				}
			}
			return true
		})
	}
	return vs
}

// checkNames: outside tests and r.namesFree's folders, a string literal
// that is a name names lists, whole, is in its owner's package: one
// anywhere else is code of its own for what that package owns.
func checkNames(m *module, r rules) []violation {
	var vs []violation
	for _, f := range m.files {
		if f.test || slices.ContainsFunc(r.namesFree, func(d string) bool { return f.dir == d || strings.HasPrefix(f.dir, d+"/") }) {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok {
				return true
			}
			if v, ok := literal(lit); ok {
				if o, named := r.names[v]; named && f.dir != o.dir {
					vs = append(vs, violation{ruleOwners, m.pos(lit), fmt.Sprintf("is %q, a name %s owns, so use %s", v, o.dir, o.use)})
				}
			}
			return true
		})
	}
	return vs
}

// checkSeparators: outside tests, a call to package strings with an
// argument that separators lists, as a literal or a constant, is in its
// owner's package: one anywhere else splits or joins what that package
// owns.
func checkSeparators(m *module, r rules) []violation {
	var vs []violation
	c := stringConsts(m)
	for _, f := range m.files {
		pkg := importName(f, "strings")
		if f.test || pkg == "" {
			continue
		}
		calls(f, func(call *ast.CallExpr) {
			if !strings.HasPrefix(callName(call), pkg+".") {
				return
			}
			for _, a := range call.Args {
				v, ok := c.value(m, f, a)
				if o, owned := r.separators[v]; ok && owned && f.dir != o.dir {
					vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("splits or joins on %q: %s owns that, so use %s", v, o.dir, o.use)})
				}
			}
		})
	}
	return vs
}

// checkPaths: outside tests and pathsOwner, no path is built in devvm's
// data or config folder: no filepath.Join onto a Data or Config field,
// and no os.MkdirTemp or os.CreateTemp there with a name of its own (a
// literal), as each would be a file the layout's owner doesn't list.
func checkPaths(m *module, r rules) []violation {
	var vs []violation
	for _, f := range m.files {
		if f.test || f.dir == r.pathsOwner {
			continue
		}
		join, osName := importName(f, "path/filepath")+".Join", importName(f, "os")
		calls(f, func(call *ast.CallExpr) {
			name := callName(call)
			if len(call.Args) == 0 || (name != join && name != osName+".MkdirTemp" && name != osName+".CreateTemp") {
				return
			}
			sel, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Data" && sel.Sel.Name != "Config") {
				return
			}
			if name != join {
				if len(call.Args) < 2 {
					return
				}
				if _, lit := literal(call.Args[1]); !lit {
					return
				}
			}
			vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("builds a path in %s: name it in %s", sel.Sel.Name, r.pathsOwner)})
		})
	}
	return vs
}

// callsMethod reports whether call calls a method named name: x.name(…)
// on any expression x.
func callsMethod(call *ast.CallExpr, name string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name
}

// checkCallers: outside tests and its own package, a function callers
// lists is called only in its file, as pkg.Name for the name the file
// imports its package as. A method, listed as pkg.Type.Name, is called
// only there by its name, on any value: its name is its type's alone in
// the module. A listed file that no longer calls it fails.
func checkCallers(m *module, r rules) []violation {
	var vs []violation
	called := map[string]bool{}
	for _, fn := range slices.Sorted(maps.Keys(r.callers)) {
		dir, name := strings.TrimSuffix(fn, path.Ext(fn)), strings.TrimPrefix(path.Ext(fn), ".")
		method := path.Ext(dir) != ""
		if method {
			dir = strings.TrimSuffix(dir, path.Ext(dir))
		}
		owner := r.callers[fn]
		for _, f := range m.files {
			pkg := importName(f, m.path+"/"+dir)
			if f.test || f.dir == dir || (pkg == "" && !method) {
				continue
			}
			calls(f, func(call *ast.CallExpr) {
				if method && !callsMethod(call, name) || !method && callName(call) != pkg+"."+name {
					return
				}
				called[fn] = called[fn] || f.name == owner
				if f.name != owner {
					vs = append(vs, violation{ruleOwners, m.pos(call), fmt.Sprintf("calls %s: only %s does, so find it there", path.Base(dir)+"."+name, owner)})
				}
			})
		}
		if !called[fn] {
			vs = append(vs, violation{ruleOwners, "callers", owner + " doesn't call " + fn + ": take it out"})
		}
	}
	return vs
}
