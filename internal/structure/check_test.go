package structure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The rules' names, as failures start.
const (
	ruleMethods = "max-methods"
	ruleInit    = "no-init"
	ruleVars    = "package-vars"
	ruleLayers  = "layers"
	ruleOwners  = "owners"
)

// rules is what check holds a module to.
type rules struct {
	maxMethods  int              // the most methods a type may have
	largeTypes  map[string]large // types allowed more, by "folder.Type"
	allowedVars map[string]string
	layers      []layer
	// commands are the host commands one package runs: a command's name,
	// then the package folder that runs it.
	commands map[string]string
	// execSites are the files that make host commands, from the module's
	// root, each with why; nil checks none.
	execSites map[string]string
	// guardOwner is the one file that asserts a backend's type (backend.As):
	// elsewhere a type assertion to a type of a backend package fails;
	// "" checks none.
	guardOwner string
	// envOwner is the one file that reads devvm's DEVVM_ variables, and
	// envSettings the variables read elsewhere, by name.
	envOwner    string
	envSettings map[string]envSetting
	// envOwners are other variables one file reads: a variable's name,
	// then the file, from the module's root.
	envOwners map[string]string
	// literals is text only one package's string literals hold, outside
	// tests: what a copy of what that package owns would hold, then the
	// package folder and what to use.
	literals map[string]literalOwner
	// names are what only one package's string literals may be, whole,
	// outside tests and namesFree's folders (and theirs below): a name
	// that package owns, then the package folder and what to use.
	names     map[string]literalOwner
	namesFree []string
	// separators are what only one package's calls to package strings
	// split or join on, outside tests: the separator, then the package
	// folder and what to use.
	separators map[string]literalOwner
	// pathsOwner is the one package that names what's in devvm's data
	// and config folders (paths.Dirs's Data and Config).
	pathsOwner string
	// callers are functions only one file calls outside their package and
	// tests: the function, as its package's folder then .Name, then the
	// file, from the module's root.
	callers map[string]string
}

// A literalOwner is the package whose string literals alone hold a text,
// and what others use instead.
type literalOwner struct {
	dir, use string
}

// An envSetting is a DEVVM_ variable read outside envOwner: the files
// that read it, from the module's root, and why.
type envSetting struct {
	files []string
	why   string
}

// A large type may have more than maxMethods: at most max, its count
// when listed, so it never grows.
type large struct {
	max int
	why string
}

// A layer is a rule on imports, checked on non-test files: packages
// matching from but not exceptFrom import no package matching to but not
// exceptTo. Patterns are package folders from the module's root; "x/..."
// is x and every package below it, "..." every package.
type layer struct {
	name       string
	from       []string
	exceptFrom []string
	to         []string
	exceptTo   []string
}

// A violation is one place that breaks a rule.
type violation struct {
	rule string
	pos  string
	text string
}

func (v violation) String() string { return v.rule + ": " + v.pos + ": " + v.text }

// A srcFile is one parsed Go file.
type srcFile struct {
	name string // its path from the module's root
	dir  string // its package's folder from the module's root ("." for the root)
	test bool
	ast  *ast.File
}

// A module is a module's path and its Go files.
type module struct {
	path  string
	fset  *token.FileSet
	files []srcFile
}

// pos is where n is, as file:line.
func (m *module) pos(n ast.Node) string {
	p := m.fset.Position(n.Pos())
	return p.Filename + ":" + strconv.Itoa(p.Line)
}

// load parses every Go file in fsys, every platform's, but generated ones
// and those in hidden folders, testdata, docs and node_modules.
func load(fsys fs.FS) (*module, error) {
	gomod, err := fs.ReadFile(fsys, "go.mod")
	if err != nil {
		return nil, err
	}
	m := &module{fset: token.NewFileSet()}
	for line := range strings.Lines(string(gomod)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			m.path = strings.TrimSpace(p)
		}
	}
	if m.path == "" {
		return nil, fmt.Errorf("go.mod: no module line")
	}
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if base := d.Name(); name != "." && (strings.HasPrefix(base, ".") || base == "testdata" || base == "docs" || base == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(m.fset, name, src, parser.ParseComments)
		if err != nil {
			return err
		}
		if !ast.IsGenerated(f) {
			m.files = append(m.files, srcFile{name: name, dir: path.Dir(name), test: strings.HasSuffix(name, "_test.go"), ast: f})
		}
		return nil
	})
	return m, err
}

// check runs every rule on m, the violations sorted.
func check(m *module, r rules) []violation {
	vs := slices.Concat(checkMethods(m, r), checkInit(m), checkVars(m, r), checkLayers(m, r), checkCommands(m, r),
		checkExecSites(m, r), checkGuard(m, r), checkEnv(m, r), checkLiterals(m, r), checkNames(m, r), checkSeparators(m, r),
		checkPaths(m, r), checkCallers(m, r))
	slices.SortFunc(vs, func(a, b violation) int { return strings.Compare(a.String(), b.String()) })
	return vs
}

// typeName is the name of the type a receiver is on: T for T, *T, T[K].
func typeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// checkMethods: no type has more than maxMethods methods, but a large
// type, which has no more than its max and is still large. An
// interface's are the methods it declares; one it embeds has its own.
func checkMethods(m *module, r rules) []violation {
	type key struct{ dir, pkg, typ string }
	counts := map[key]int{}
	where := map[key]string{}
	for _, f := range m.files {
		for _, d := range f.ast.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					ts, ok := s.(*ast.TypeSpec)
					if !ok {
						continue
					}
					k := key{f.dir, f.ast.Name.Name, ts.Name.Name}
					where[k] = m.pos(ts)
					if it, ok := ts.Type.(*ast.InterfaceType); ok {
						for _, fl := range it.Methods.List {
							counts[k] += len(fl.Names)
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				k := key{f.dir, f.ast.Name.Name, typeName(d.Recv.List[0].Type)}
				counts[k]++
				if _, ok := where[k]; !ok {
					where[k] = m.pos(d)
				}
			}
		}
	}
	var vs []violation
	seen := map[string]bool{}
	for k, n := range counts {
		name := k.dir + "." + k.typ
		l, ok := r.largeTypes[name]
		if !ok || strings.HasSuffix(k.pkg, "_test") {
			if n > r.maxMethods {
				vs = append(vs, violation{ruleMethods, where[k], fmt.Sprintf("type %s has %d methods, more than %d: split it by what its methods do", k.typ, n, r.maxMethods)})
			}
			continue
		}
		seen[name] = true
		switch {
		case n > l.max:
			vs = append(vs, violation{ruleMethods, where[k], fmt.Sprintf("type %s has %d methods, more than its %d in largeTypes: split it by what its methods do", k.typ, n, l.max)})
		case n <= r.maxMethods:
			vs = append(vs, violation{ruleMethods, where[k], fmt.Sprintf("type %s has %d methods: take it out of largeTypes", k.typ, n)})
		case n < l.max:
			vs = append(vs, violation{ruleMethods, where[k], fmt.Sprintf("type %s has %d methods: lower its max in largeTypes to that", k.typ, n)})
		}
	}
	for name := range r.largeTypes {
		if !seen[name] {
			vs = append(vs, violation{ruleMethods, "largeTypes", name + " has no methods: take it out"})
		}
	}
	return vs
}

// checkInit: no func init(), tests included.
func checkInit(m *module) []violation {
	var vs []violation
	for _, f := range m.files {
		for _, d := range f.ast.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "init" {
				vs = append(vs, violation{ruleInit, m.pos(fd), "func init: set it up where it's used, or in a constructor"})
			}
		}
	}
	return vs
}

// callName is a call's function as pkg.Func, or "".
func callName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	fun := call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok { // sync.OnceValue[T]
		fun = ix.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if x, ok := sel.X.(*ast.Ident); ok {
		return x.Name + "." + sel.Sel.Name
	}
	return ""
}

// madeOnce is whether a call's result is made once and never changes: a
// compiled regexp or template, or a sync.Once* function.
func madeOnce(call string) bool {
	switch call {
	case "regexp.MustCompile", "template.Must", "sync.OnceValue", "sync.OnceValues", "sync.OnceFunc":
		return true
	}
	return false
}

// embedded is whether a doc comment has a //go:embed directive.
func embedded(docs ...*ast.CommentGroup) bool {
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		for _, c := range doc.List {
			if strings.HasPrefix(c.Text, "//go:embed ") {
				return true
			}
		}
	}
	return false
}

// varAllowed is whether a package-level var may be by what it is: a blank
// one (an interface check), an embedded file, an error sentinel (Err* or
// err*) or what madeOnce makes.
func varAllowed(name string, value ast.Expr, embed bool) bool {
	if name == "_" || embed {
		return true
	}
	call := callName(value)
	if madeOnce(call) {
		return true
	}
	sentinel := call == "errors.New" || call == "fmt.Errorf"
	return sentinel && (strings.HasPrefix(name, "Err") || strings.HasPrefix(name, "err"))
}

// checkVars: package-level vars, tests' too, are constants' kin: what
// varAllowed allows, or what allowedVars lists.
func checkVars(m *module, r rules) []violation {
	var vs []violation
	seen := map[string]bool{}
	for _, f := range m.files {
		for _, d := range f.ast.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, s := range gd.Specs {
				spec, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range spec.Names {
					var value ast.Expr
					if len(spec.Values) == len(spec.Names) {
						value = spec.Values[i]
					}
					key := f.dir + "." + name.Name
					if _, ok := r.allowedVars[key]; ok {
						seen[key] = true
						continue
					}
					if !varAllowed(name.Name, value, embedded(gd.Doc, spec.Doc)) {
						vs = append(vs, violation{ruleVars, m.pos(name), "var " + name.Name + ": make it a field, a const or a local, or list it in allowedVars with why"})
					}
				}
			}
		}
	}
	for key := range r.allowedVars {
		if !seen[key] {
			vs = append(vs, violation{ruleVars, "allowedVars", key + " is no package-level var: take it out"})
		}
	}
	return vs
}

// matches is whether a package folder matches any of patterns.
func matches(dir string, patterns []string) bool {
	for _, p := range patterns {
		if p == "..." || p == dir {
			return true
		}
		if base, ok := strings.CutSuffix(p, "/..."); ok && (dir == base || strings.HasPrefix(dir, base+"/")) {
			return true
		}
	}
	return false
}

// checkLayers: no non-test file imports what a layer forbids. Tests may
// wire packages together.
func checkLayers(m *module, r rules) []violation {
	var vs []violation
	for _, f := range m.files {
		if f.test {
			continue
		}
		for _, imp := range f.ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			to, ok := strings.CutPrefix(p, m.path+"/")
			if !ok {
				if p != m.path {
					continue
				}
				to = "."
			}
			for _, l := range r.layers {
				if matches(f.dir, l.from) && !matches(f.dir, l.exceptFrom) && matches(to, l.to) && !matches(to, l.exceptTo) {
					vs = append(vs, violation{ruleLayers, m.pos(imp), fmt.Sprintf("%s imports %s: %s", f.dir, to, l.name)})
				}
			}
		}
	}
	return vs
}
