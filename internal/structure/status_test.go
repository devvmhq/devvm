package structure

import (
	"go/ast"
	"strings"
)

// ruleStatus is checkStatus' name, as its failures start.
const ruleStatus = "status-lines"

// slowCall is what slow work n is, "" for none: a host command made, host
// git, a walk of a folder, a lookup of a volume, a request over HTTP, or
// one of a backend's commands run outside a step (its Runner's Output or
// Attach). A backend's own methods, such as Exec, aren't told apart by
// name: TestLaunchStatusLines checks a launch's.
func slowCall(f srcFile, n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	name := callName(call)
	for _, p := range []struct{ path, what string }{
		{"os/exec", "makes a host command"},
		{"github.com/devvmhq/devvm/internal/hostgit", "runs host git"},
		{"github.com/devvmhq/devvm/internal/fetch", "makes a request over HTTP"},
	} {
		pkg := importName(f, p.path)
		if pkg == "" || !strings.HasPrefix(name, pkg+".") {
			continue
		}
		fn := strings.TrimPrefix(name, pkg+".")
		switch p.path {
		case "os/exec":
			if fn == "Command" || fn == "CommandContext" {
				return p.what
			}
		case "github.com/devvmhq/devvm/internal/fetch":
			if fn == "Open" || fn == "Resolve" || fn == "Get" || fn == "Download" {
				return p.what
			}
		default:
			if ast.IsExported(fn) {
				return p.what
			}
		}
	}
	for _, c := range []struct{ path, fn, what string }{
		{"io/fs", "WalkDir", "walks a folder"},
		{"path/filepath", "Walk", "walks a folder"},
		{"path/filepath", "WalkDir", "walks a folder"},
		{"github.com/devvmhq/devvm/internal/hostsafety", "Walk", "walks a folder"},
		{"github.com/devvmhq/devvm/internal/safefile", "Walk", "walks a folder"},
		{"github.com/devvmhq/devvm/internal/platform", "VolumeUUID", "looks up a volume"},
	} {
		if pkg := importName(f, c.path); pkg != "" && name == pkg+"."+c.fn {
			return c.what
		}
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Output" || sel.Sel.Name == "Attach") {
		if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Runner" {
			return "runs a backend's command outside a step"
		}
	}
	return ""
}

// showsStatus is whether n shows a status line: a Reporter's Status, a
// logged command's step (LoggedCmd), or a backend Runner's Run, which is
// one.
func showsStatus(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch {
	case callsMethod(call, "Status") && len(call.Args) == 1, callsMethod(call, "LoggedCmd") && len(call.Args) == 3:
		return true
	case callsMethod(call, "Run") && len(call.Args) == 3:
		x, ok := call.Fun.(*ast.SelectorExpr).X.(*ast.SelectorExpr)
		return ok && x.Sel.Name == "Runner"
	}
	return false
}

// slowSites are f's functions, by siteName, that do slow work (slowCall)
// and show no status line of their own (showsStatus), each with its first
// slow call; none in a folder free lists.
func slowSites(free map[string]string) func(srcFile) map[string]drop {
	return func(f srcFile) map[string]drop {
		for dir := range free {
			if f.dir == dir || strings.HasPrefix(f.dir, dir+"/") {
				return nil
			}
		}
		return sitesWhereIn(f, func(fd *ast.FuncDecl) func(ast.Node) string {
			shows := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				shows = shows || showsStatus(n)
				return !shows
			})
			return func(n ast.Node) string {
				if shows {
					return ""
				}
				return slowCall(f, n)
			}
		})
	}
}

// checkStatus: outside tests and the folders statusFree lists, a function
// that does slow work, a host command, host git, a walk of a folder, a
// lookup of a volume, a request over HTTP or one of a backend's commands
// outside a step, shows a status line itself (a Reporter's
// Status, or a logged command's step), so the user sees what devvm waits
// for, unless statusSites lists it with why: under its caller's status
// line, on the terminal, in a process with no UI, or quick. A listed
// function that does none fails. A nil statusSites checks none.
func checkStatus(m *module, r rules) []violation {
	if r.statusSites == nil {
		return nil
	}
	return siteRule{ruleStatus, "statusSites", r.statusSites, slowSites(r.statusFree), "show a status line (Status, or LoggedCmd's step)", "no slow work without a status line"}.check(m)
}

// statusCases break and keep status-lines.
func statusCases() []ruleCase {
	file := func(body string) map[string]string {
		return map[string]string{"a/a.go": "package a\n\nimport (\n\t\"context\"\n\tx \"os/exec\"\n\t\"path/filepath\"\n)\n\n" +
			"type R interface{ Status(string) func() }\n\nvar _ = filepath.Walk\n\nvar _ context.Context\n\n" + body}
	}
	on := rules{statusSites: map[string]string{}}
	return []ruleCase{
		{
			name:  "a host command, its import renamed, with no status line",
			files: file("func run(ctx context.Context) error { return x.CommandContext(ctx, \"true\").Run() }\n"),
			r:     on,
			want:  &want{ruleStatus, "list a/a.go: run in statusSites"},
		},
		{
			name:  "a walk in a method",
			files: file("type T struct{}\n\nfunc (T) walk(d string) { _ = filepath.Walk(d, nil) }\n"),
			r:     on,
			want:  &want{ruleStatus, "list a/a.go: T.walk in statusSites"},
		},
		{
			name:  "a Lima command outside a step",
			files: file("type L struct{ Runner interface{ Output(*x.Cmd) ([]byte, error) } }\n\nfunc (l L) list(c *x.Cmd) { _, _ = l.Runner.Output(c) }\n"),
			r:     on,
			want:  &want{ruleStatus, "list a/a.go: L.list in statusSites"},
		},
		{
			name:  "a host command under a status line",
			files: file("func run(ctx context.Context, r R) error {\n\tdefer r.Status(\"x\")()\n\treturn x.CommandContext(ctx, \"true\").Run()\n}\n"),
			r:     on,
		},
		{
			name:  "a host command as a logged command's step",
			files: file("type U interface{ LoggedCmd(context.Context, string, *x.Cmd) error }\n\nfunc run(ctx context.Context, u U) error { return u.LoggedCmd(ctx, \"s\", x.CommandContext(ctx, \"true\")) }\n"),
			r:     on,
		},
		{
			name:  "a listed function",
			files: file("func run(ctx context.Context) error { return x.CommandContext(ctx, \"true\").Run() }\n"),
			r:     rules{statusSites: map[string]string{"a/a.go: run": "under its caller's status line"}},
		},
		{
			name:  "a listed function with nothing slow",
			files: file("func run() {}\n"),
			r:     rules{statusSites: map[string]string{"a/a.go: run": "under its caller's status line"}},
			want:  &want{ruleStatus, "a/a.go: run has no slow work without a status line: take it out"},
		},
		{
			name:  "a folder free of the rule",
			files: file("func run(ctx context.Context) error { return x.CommandContext(ctx, \"true\").Run() }\n"),
			r:     rules{statusSites: map[string]string{}, statusFree: map[string]string{"a": "a process with no UI"}},
		},
		{
			name:  "a host command in a test",
			files: map[string]string{"a/a_test.go": "package a\n\nimport \"os/exec\"\n\nfunc run() error { return exec.Command(\"true\").Run() }\n"},
			r:     on,
		},
	}
}
