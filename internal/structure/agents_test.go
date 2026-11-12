package structure

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// A switchOwner is the package that alone compares with, chooses by or keys
// on a name, but for files named for it: word is what such a file's name
// holds, as one of its words (claude in claude.go, agentlogin_claude.go).
type switchOwner struct {
	dir, word, use string
}

// An everyOwner is a list of names, every one of which a literal list holds
// only in its owner's package: elsewhere it's a copy of the owner's list.
type everyOwner struct {
	names    []string
	dir, use string
}

// fileWords are the words of a file's name, without its extension and its
// _test, split at _ and -.
func fileWords(name string) []string {
	base := strings.TrimSuffix(strings.TrimSuffix(path.Base(name), ".go"), "_test")
	return strings.FieldsFunc(base, func(r rune) bool { return r == '_' || r == '-' })
}

// checkSwitches: outside tests, the owner's package and the files named
// for it, no code compares with a name switches lists (==, != or package
// strings' HasPrefix, HasSuffix, EqualFold and Contains), has it as a
// switch's case, looks for it with slices.Contains or slices.Index, looks
// it up in a map or keys a map literal on it, as a constant, even one
// named after another (a literal name is checkNames'): code of one agent's
// own goes in a file named for it, and everything else asks its Def.
// switchFree's constants pass in their files, and one that passes nothing
// there fails.
func checkSwitches(m *module, r rules) []violation {
	var vs []violation
	c := stringConsts(m)
	used := map[string]bool{}
	for _, f := range m.files {
		if f.test {
			continue
		}
		words := fileWords(f.name)
		slicesPkg, stringsPkg := importName(f, "slices"), importName(f, "strings")
		flag := func(e ast.Expr, how string) {
			if _, lit := e.(*ast.BasicLit); lit {
				return // a literal name is checkNames'
			}
			v, ok := c.value(m, f, e)
			o, owned := r.switches[v]
			if !ok || !owned || f.dir == o.dir || slices.Contains(words, o.word) {
				return
			}
			if free := f.name + ": " + c.name(m, f, e); r.switchFree[free] != "" {
				used[free] = true
				return
			}
			vs = append(vs, violation{ruleOwners, m.pos(e), fmt.Sprintf("%s %q: %s", how, v, o.use)})
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.EQL || x.Op == token.NEQ {
					flag(x.X, "compares with")
					flag(x.Y, "compares with")
				}
			case *ast.IndexExpr:
				flag(x.Index, "looks up")
			case *ast.SwitchStmt:
				if x.Tag != nil {
					for _, s := range x.Body.List {
						for _, e := range s.(*ast.CaseClause).List {
							flag(e, "has a case for")
						}
					}
				}
			case *ast.CallExpr:
				name := callName(x)
				if slicesPkg != "" && (name == slicesPkg+".Contains" || name == slicesPkg+".Index") && len(x.Args) == 2 {
					flag(x.Args[1], "looks for")
				}
				if stringsPkg != "" && slices.Contains([]string{"HasPrefix", "HasSuffix", "EqualFold", "Contains"}, strings.TrimPrefix(name, stringsPkg+".")) &&
					strings.HasPrefix(name, stringsPkg+".") {
					for _, a := range x.Args {
						flag(a, "compares with")
					}
				}
			case *ast.CompositeLit:
				if _, isMap := x.Type.(*ast.MapType); isMap {
					for _, e := range x.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							flag(kv.Key, "keys a map on")
						}
					}
				}
			}
			return true
		})
	}
	for _, free := range slices.Sorted(maps.Keys(r.switchFree)) {
		if !used[free] {
			vs = append(vs, violation{ruleOwners, "switchFree", free + " passes nothing: take it out"})
		}
	}
	return vs
}

// checkEvery: tests included, outside every's owner, no literal list (a
// slice's, an array's or a call's arguments) holds every one of its names,
// as literals or constants: that's a copy of the owner's list, which a new
// name leaves stale.
func checkEvery(m *module, r rules) []violation {
	if len(r.every.names) == 0 {
		return nil
	}
	var vs []violation
	c := stringConsts(m)
	for _, f := range m.files {
		if f.dir == r.every.dir {
			continue
		}
		check := func(n ast.Node, elts []ast.Expr) {
			var got []string
			for _, e := range elts {
				if v, ok := c.value(m, f, e); ok {
					got = append(got, v)
				}
			}
			if !slices.ContainsFunc(r.every.names, func(n string) bool { return !slices.Contains(got, n) }) {
				vs = append(vs, violation{ruleOwners, m.pos(n), "lists every one of " + strings.Join(r.every.names, ", ") + ": a copy of " + r.every.dir + "'s list, so use " + r.every.use})
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				check(x, x.Elts)
			case *ast.CallExpr:
				check(x, x.Args)
			}
			return true
		})
	}
	return vs
}

// scriptLists are the lines of a CLI test script, name, that list two or
// more of names joined with a comma and a space, as devvm's messages list
// a choice's names: a copy of a list the registry gives the scripts
// (AGENT_CHOICES, LOGIN_AGENTS, KEY_AGENTS), which a new agent leaves
// stale. A list for a flag, joined with a comma alone, is the script's own
// choice.
func scriptLists(name, text string, names []string) []violation {
	alt := strings.Join(names, "|")
	list := regexp.MustCompile(`\b(` + alt + `), (` + alt + `)\b`)
	var vs []violation
	for i, line := range strings.Split(text, "\n") {
		if list.MatchString(line) {
			vs = append(vs, violation{
				ruleOwners, fmt.Sprintf("%s:%d", name, i+1),
				"lists agents as a message does: use $AGENT_CHOICES, $LOGIN_AGENTS or $KEY_AGENTS",
			})
		}
	}
	return vs
}
