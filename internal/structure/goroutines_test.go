package structure

import (
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// ruleGoroutines is checkGoroutineSites's name, as its failures start.
const ruleGoroutines = "goroutines"

// detached starts a goroutine site's entry when nothing waits for it:
// then why it may run on (goroutineSites).
const detached = "detached: "

// leaktestPkg is the package whose Main a package that starts goroutines
// calls in its tests' TestMain.
const leaktestPkg = "internal/leaktest"

// siteName is the function a goroutine site is in, as goroutineSites names
// it: "file: Func", or "file: Type.Method", from the module's root; a
// function literal's is the function it's in, or the package-level var
// it's the value of.
func siteName(f srcFile, fd *ast.FuncDecl) string {
	name := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		name = typeName(fd.Recv.List[0].Type) + "." + name
	}
	return f.name + ": " + name
}

// startsGoroutine is whether n starts a goroutine: a go statement, a wait
// group's Go (wg.Go(f)), or time's or context's AfterFunc, whose function
// runs in a goroutine of its own. Goroutines the standard library starts
// otherwise, such as an HTTP server's, are leaktest's to find.
func startsGoroutine(f srcFile, n ast.Node) bool {
	switch n := n.(type) {
	case *ast.GoStmt:
		return true
	case *ast.CallExpr:
		if callsMethod(n, "Go") && len(n.Args) == 1 {
			return true
		}
		name := callName(n)
		for _, p := range []string{"time", "context"} {
			if pkg := importName(f, p); pkg != "" && name == pkg+".AfterFunc" && len(n.Args) == 2 {
				return true
			}
		}
	}
	return false
}

// goroutineSites are f's functions that start goroutines, by siteName,
// each with where the first one is: its functions' bodies, and the
// function literals its package-level vars hold.
func goroutineSites(f srcFile) map[string]ast.Node {
	sites := map[string]ast.Node{}
	walk := func(site string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			if _, seen := sites[site]; !seen && startsGoroutine(f, n) {
				sites[site] = n
			}
			return true
		})
	}
	for _, d := range f.ast.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				walk(siteName(f, d), d.Body)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, s := range d.Specs {
				spec := s.(*ast.ValueSpec)
				for i, v := range spec.Values {
					name := spec.Names[min(i, len(spec.Names)-1)].Name
					walk(f.name+": "+name, v)
				}
			}
		}
	}
	return sites
}

// callsLeaktestMain is whether a test file of f's calls leaktest.Main.
func callsLeaktestMain(m *module, f srcFile) bool {
	pkg := importName(f, m.path+"/"+leaktestPkg)
	found := false
	if pkg != "" {
		calls(f, func(call *ast.CallExpr) { found = found || callName(call) == pkg+".Main" })
	}
	return found
}

// checkGoroutineSites: outside tests, a function that starts a goroutine
// is in goroutineSites, with what waits for its goroutines to end, or
// "detached: " and why nothing does; a listed function that starts none
// fails. And a package that starts goroutines calls leaktest.Main in its
// tests' TestMain, which finds those that outlive its tests.
func checkGoroutineSites(m *module, r rules) []violation {
	if r.goroutineSites == nil {
		return nil
	}
	var vs []violation
	seen := map[string]bool{}
	starts := map[string]string{} // a package's folder, then where it first starts one
	checked := map[string]bool{}  // the folders whose tests call leaktest.Main
	for _, f := range m.files {
		if f.test {
			checked[f.dir] = checked[f.dir] || callsLeaktestMain(m, f)
			continue
		}
		for site, n := range goroutineSites(f) {
			seen[site] = true
			if _, ok := starts[f.dir]; !ok {
				starts[f.dir] = m.pos(n)
			}
			if _, ok := r.goroutineSites[site]; !ok {
				vs = append(vs, violation{ruleGoroutines, m.pos(n), "starts a goroutine: list " + site + " in goroutineSites with what waits for it to end, or " + strings.TrimSpace(detached) + " and why nothing does"})
			}
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(starts)) {
		if dir != leaktestPkg && !checked[dir] {
			vs = append(vs, violation{ruleGoroutines, starts[dir], dir + " starts goroutines: call leaktest.Main in its tests' TestMain"})
		}
	}
	for _, site := range slices.Sorted(maps.Keys(r.goroutineSites)) {
		why := r.goroutineSites[site]
		switch {
		case !seen[site]:
			vs = append(vs, violation{ruleGoroutines, "goroutineSites", site + " starts no goroutine: take it out"})
		case why == "" || why == strings.TrimSpace(detached) || strings.HasPrefix(why, detached) && strings.TrimSpace(strings.TrimPrefix(why, detached)) == "":
			vs = append(vs, violation{ruleGoroutines, "goroutineSites", site + ": say what waits for its goroutines, or " + strings.TrimSpace(detached) + " and why nothing does"})
		}
	}
	return vs
}

// goroutineCases break and keep goroutineSites.
func goroutineCases() []ruleCase {
	sites := func(s map[string]string) rules { return rules{goroutineSites: s} }
	const src = "package a\n\nimport \"sync\"\n\ntype T struct{ wg sync.WaitGroup }\n\n" +
		"func run() { go func() {}() }\n\nfunc (t *T) serve() { t.wg.Go(func() {}) }\n"
	// leaktested is a's test that calls leaktest.Main.
	const leaktested = "package a\n\nimport (\n\t\"testing\"\n\n\t\"example.com/m/internal/leaktest\"\n)\n\n" +
		"func TestMain(m *testing.M) { leaktest.Main(m) }\n"
	with := func(files map[string]string) map[string]string {
		files["a/main_test.go"] = leaktested
		return files
	}
	return []ruleCase{
		{
			name:  "goroutines in listed functions",
			files: with(map[string]string{"a/a.go": src}),
			r:     sites(map[string]string{"a/a.go: run": "run's caller, through done", "a/a.go: T.serve": detached + "it ends with the process"}),
		},
		{
			name:  "a go statement in a function not listed",
			files: with(map[string]string{"a/a.go": src}),
			r:     sites(map[string]string{"a/a.go: T.serve": "Close"}),
			want:  &want{ruleGoroutines, "starts a goroutine: list a/a.go: run in goroutineSites"},
		},
		{
			name:  "a wait group's Go in a method not listed",
			files: with(map[string]string{"a/a.go": src}),
			r:     sites(map[string]string{"a/a.go: run": "its caller"}),
			want:  &want{ruleGoroutines, "starts a goroutine: list a/a.go: T.serve in goroutineSites"},
		},
		{
			name: "an AfterFunc not listed",
			files: with(map[string]string{"a/a.go": "package a\n\nimport (\n\t\"context\"\n\tt \"time\"\n)\n\n" +
				"func run(ctx context.Context) { context.AfterFunc(ctx, func() {}) }\n\nfunc tick(f func()) { t.AfterFunc(0, f) }\n"}),
			r:    sites(map[string]string{"a/a.go: run": "its stop"}),
			want: &want{ruleGoroutines, "starts a goroutine: list a/a.go: tick in goroutineSites"},
		},
		{
			name:  "a package-level function literal not listed",
			files: with(map[string]string{"a/a.go": "package a\n\nvar _ = func() { go func() {}() }\n"}),
			r:     sites(map[string]string{}),
			want:  &want{ruleGoroutines, "starts a goroutine: list a/a.go: _ in goroutineSites"},
		},
		{
			name:  "goroutines without leaktest.Main",
			files: map[string]string{"a/a.go": src, "a/a_test.go": "package a\n"},
			r:     sites(map[string]string{"a/a.go: run": "its caller", "a/a.go: T.serve": "Close"}),
			want:  &want{ruleGoroutines, "a starts goroutines: call leaktest.Main in its tests' TestMain"},
		},
		{
			name:  "a listed function starting none",
			files: with(map[string]string{"a/a.go": "package a\n\nfunc run() {}\n"}),
			r:     sites(map[string]string{"a/a.go: run": "its caller"}),
			want:  &want{ruleGoroutines, "a/a.go: run starts no goroutine: take it out"},
		},
		{
			name:  "detached without why",
			files: with(map[string]string{"a/a.go": "package a\n\nfunc run() { go run() }\n"}),
			r:     sites(map[string]string{"a/a.go: run": detached}),
			want:  &want{ruleGoroutines, "a/a.go: run: say what waits for its goroutines"},
		},
		{
			name:  "a goroutine in a test",
			files: map[string]string{"a/a_test.go": "package a\n\nfunc run() { go run() }\n"},
			r:     sites(map[string]string{}),
		},
	}
}
