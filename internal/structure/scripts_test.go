package structure

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// shells are the programs devvm runs a script with as "PROGRAM -c
// SCRIPT" (or -lc), and the shell shellcheck checks it as.
func shells() map[string]string {
	return map[string]string{"sh": "sh", "/bin/sh": "sh", "bash": "bash"}
}

// dynamicScripts are the call sites whose script devvm builds at run
// time, so the structure test can't read it: the file, then the
// script's expression as Go writes it, and why it's fine or where it's
// checked instead. Every other script is a constant or a literal, which
// TestEmbeddedScripts runs shellcheck on, and a new site that's neither
// fails until it's listed here.
func dynamicScripts() map[string]string {
	return map[string]string{
		"internal/backend/lima/lima.go " + `"cd " + shell.Quote(dir) + " && exec " + shell.QuoteJoin(argv)`: "Exec's own wrapper: a cd and an exec of words each quoted (shell.Quote)",
		"internal/backend/lima/packages.go " + `"rm -rf " + d + " && mkdir -p " + d + " && tar -x -C " + d`: "a fixed command around a quoted path",
		"internal/backend/lima/packages.go " + `"mkdir -p " + d + "/out && tar -c -C " + d + "/out ."`:      "a fixed command around a quoted path",
		"internal/backend/lima/remount.go script":                                                           "a fixed umount and mount around a quoted path, which start_test compares word for word",
		"internal/backend/lima/remount.go check":                                                            "a fixed mountpoint test around quoted paths",
		"internal/vm/mountcache.go script":                                                                  "a fixed mountpoint test around a quoted path",
		"internal/vm/agentsave.go agentSaveScript(agents)":                                                  "built from the agents' registry: internal/vm's TestBuiltScriptsShellcheck checks it",
		"internal/vm/lifecycle.go historyExport()":                                                          "built from the agents' registry: internal/vm's TestBuiltScriptsShellcheck checks it",
		"internal/vm/lifecycle.go historyImport()":                                                          "built from the agents' registry: internal/vm's TestBuiltScriptsShellcheck checks it",
		"internal/devcontainer/devcontainer.go s":                                                           "a devcontainer.json's own command, the user's, not devvm's",
		"internal/vm/opencode.go string(script)":                                                            "assets/guest/opencode.sh, which lint:code's shellcheck checks with the assets",
		"internal/vm/paste.go script":                                                                       "assets/guest/paste.sh, through pasteArgv, which lint:code's shellcheck checks with the assets",
	}
}

// embeddedScript is a script devvm's Go code runs with a shell (shells):
// where, which shell, and its text.
type embeddedScript struct {
	pos, shell, text string
}

// embeddedScripts finds every call site in m's non-test Go code, the
// stubs' aside, that runs a script with a shell: a []string literal or a
// call's arguments with "sh" (or "bash", "/bin/sh") then "-c" (or "-lc")
// then the script. It returns the scripts it can read, a string
// constant, a literal or a sum of them, the sites it can't, each as
// "FILE EXPRESSION" (dynamicScripts' keys), and the sites that give a
// shell a flag other than those or -s (a script on stdin), such as -ec,
// or -e then -c, whose script this would miss, each as "FILE:LINE
// SHELL FLAG".
func embeddedScripts(m *module) (scripts []embeddedScript, dynamic, odd []string) {
	c := summedConsts(m)
	for _, f := range m.files {
		if f.test || strings.HasPrefix(f.dir, "internal/stubtest") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var list []ast.Expr
			switch x := n.(type) {
			case *ast.CompositeLit:
				list = x.Elts
			case *ast.CallExpr:
				list = x.Args
			default:
				return true
			}
			for i := 0; i+1 < len(list); i++ {
				prog, _ := literal(list[i])
				flag, _ := literal(list[i+1])
				sh, ok := shells()[prog]
				if !ok || !strings.HasPrefix(flag, "-") || flag == "-s" {
					continue
				}
				if flag != "-c" && flag != "-lc" || i+2 >= len(list) {
					odd = append(odd, m.fset.Position(list[i+1].Pos()).String()+" "+prog+" "+flag)
					continue
				}
				if text, ok := scriptText(c, m, f, list[i+2]); ok {
					scripts = append(scripts, embeddedScript{pos: m.fset.Position(list[i+2].Pos()).String(), shell: sh, text: text})
				} else {
					dynamic = append(dynamic, f.name+" "+types.ExprString(list[i+2]))
				}
			}
			return true
		})
	}
	return scripts, dynamic, odd
}

// summedConsts is stringConsts with the string constants declared as a
// sum of literals and other such constants too, such as a script with a
// file's name in it.
func summedConsts(m *module) consts {
	c := stringConsts(m)
	for grew := true; grew; {
		grew = false
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
						key := f.dir + "." + name.Name
						if _, known := c[key]; known {
							continue
						}
						if v, ok := scriptText(c, m, f, spec.Values[i]); ok {
							c[key], grew = v, true
						}
					}
				}
			}
		}
	}
	return c
}

// scriptText is the script e is, when it's a string constant, a literal,
// or a sum of them.
func scriptText(c consts, m *module, f srcFile, e ast.Expr) (string, bool) {
	if b, ok := e.(*ast.BinaryExpr); ok && b.Op == token.ADD {
		l, okl := scriptText(c, m, f, b.X)
		r, okr := scriptText(c, m, f, b.Y)
		return l + r, okl && okr
	}
	if v, ok := literal(e); ok {
		return v, true
	}
	return c.value(m, f, e)
}

// embeddedScripts finds a script after -c or -lc, takes -s for one on
// stdin, and reports a shell given any other flag, whose script it would
// miss.
func TestEmbeddedScriptsFlags(t *testing.T) {
	src := "package a\n\nfunc f(run func(...string)) {\n" +
		"\trun(\"sh\", \"-c\", \"echo a\")\n\trun(\"bash\", \"-lc\", \"echo b\")\n\trun(\"bash\", \"-s\", \"--\", \"x\")\n" +
		"\trun(\"sh\", \"-ec\", \"echo c\")\n\trun(\"sh\", \"-e\", \"-c\", \"echo d\")\n\trun(\"bash\", \"-c\")\n}\n"
	fsys := fstest.MapFS{
		"go.mod":          {Data: []byte("module example.com/m\n\ngo 1.27\n")},
		"internal/a/a.go": {Data: []byte(src)},
	}
	m, err := load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	scripts, dynamic, odd := embeddedScripts(m)
	var texts []string
	for _, s := range scripts {
		texts = append(texts, s.text)
	}
	var flags []string
	for _, o := range odd {
		flags = append(flags, o[strings.LastIndexByte(o, ' ')+1:])
	}
	if !slices.Equal(texts, []string{"echo a", "echo b"}) || len(dynamic) != 0 || !slices.Equal(flags, []string{"-ec", "-e", "-c"}) {
		t.Errorf("scripts %q, dynamic %q, odd %q", texts, dynamic, odd)
	}
}

// Every shell script devvm's Go code embeds passes shellcheck, as its
// shell (sh or bash) reads it, a script the test can't read is one
// dynamicScripts lists, each still there, and no shell gets a flag that
// would hide its script from the test.
func TestEmbeddedScripts(t *testing.T) {
	m, err := load(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	scripts, dynamic, odd := embeddedScripts(m)
	for _, o := range odd {
		t.Errorf("%s: a shell given a flag other than -c, -lc or -s, so its script isn't checked: put options in the script (set -e)", o)
	}
	listed := dynamicScripts()
	for _, d := range dynamic {
		if _, ok := listed[d]; !ok {
			t.Errorf("%s: a script built at run time, which shellcheck can't read here: make it a constant, or list it in dynamicScripts with why", d)
		}
	}
	for d := range listed {
		if !slices.Contains(dynamic, d) {
			t.Errorf("dynamicScripts lists %s, which no call site is any more", d)
		}
	}
	if len(scripts) < 10 {
		t.Fatalf("found only %d scripts", len(scripts))
	}
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("no shellcheck here")
	}
	dir := t.TempDir()
	for i, s := range scripts {
		file := filepath.Join(dir, "script"+string(rune('a'+i%26))+".sh")
		if err := os.WriteFile(file, []byte(s.text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Warnings and errors: info is style, and here mostly what can't
		// be followed outside the VM, such as a sourced /etc/environment,
		// or SQL quoting inside a script's quotes.
		if out, err := exec.CommandContext(t.Context(), sc, "-s", s.shell, "-S", "warning", file).CombinedOutput(); err != nil { //nolint:gosec // shellcheck, on the test's own file
			t.Errorf("%s: shellcheck -s %s -S warning:\n%s", s.pos, s.shell, out)
		}
	}
}
