package vm

import (
	"bytes"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The parsers of what a --clone VM prints, each of which the VM's own programs can write: each
// fails closed and keeps to its bounds, whatever the VM says.

// FuzzParseCloneState: a clone's answer to the check is taken only whole, with every repo asked
// for gone or with its branches and status, at most cloneRefsMax branches in all and
// cloneWorktreesMax worktrees a repo, each worktree once, at a clean path below the project
// folder, with a commit's name or none for its HEAD; and nothing taken is more than the answer.
func FuzzParseCloneState(f *testing.F) {
	const ws = "/p/ws"
	sha := strings.Repeat("a", 40)
	f.Add([]byte(rec("refs", 0, "abc refs/heads/main\n")+rec("status", 0, "")+rec("path", 0, ".worktrees/a/api")+rec("wstatus", 0, "?? x\n")+
		rec("path", 0, ".worktrees/b/api")+rec("whead", 0, sha+"\n")+rec("wstatus", 0, "")+rec("gone", 1, "")+rec("refs", 2, "")+rec("status", 2, " M y\n")), uint8(3))
	f.Add([]byte(rec("same", 0, "a\x00b\x00")+rec("refs", 0, "")+rec("status", 0, "")), uint8(1))
	f.Add([]byte(rec("refs", 0, "")+rec("status", 0, "")+rec("path", 0, "../x")+rec("wstatus", 0, "")), uint8(1))
	f.Add([]byte(rec("refs", 0, "")+"status 0 99999999\nabc"), uint8(1))
	f.Add([]byte(rec("refs", 0, "")+rec("status", 0, "")+rec("path", 0, "a")+rec("wstatus", 0, "")+rec("path", 0, "./a")+rec("wstatus", 0, "")), uint8(1))
	f.Fuzz(func(t *testing.T, out []byte, repos uint8) {
		n := int(repos%4) + 1
		got, err := parseCloneState(out, ws, n)
		if err != nil {
			if got != nil {
				t.Fatalf("refused with %+v", got)
			}
			return
		}
		if len(got) != n {
			t.Fatalf("%d repos, asked for %d", len(got), n)
		}
		kinds := recordKinds(out)
		for i := range n {
			k := kinds[i]
			if !k["gone"] && (!k["refs"] || !k["status"]) || got[i].gone != k["gone"] {
				t.Fatalf("repo %d taken, gone %v, from records %v", i, got[i].gone, k)
			}
		}
		seen := map[string]bool{}
		refs, size := 0, 0
		for _, r := range got {
			refs += bytes.Count(r.refs, []byte("\n"))
			size += len(r.refs) + len(r.status)
			if len(r.trees) > cloneWorktreesMax {
				t.Fatalf("%d worktrees", len(r.trees))
			}
			for _, w := range r.trees {
				rel, ok := strings.CutPrefix(w.path, ws+"/")
				switch {
				case !ok || rel == "" || path.Clean(w.path) != w.path || seen[w.path]:
					t.Fatalf("a worktree at %q", w.path)
				case w.head != "" && !objectName.MatchString(w.head):
					t.Fatalf("a worktree's HEAD %q", w.head)
				}
				seen[w.path] = true
				size += len(w.status)
			}
		}
		if refs > cloneRefsMax || size > len(out) {
			t.Fatalf("%d branches and %d bytes, from %d", refs, size, len(out))
		}
	})
}

// recordKinds reads cloneState's records in out apart from parseCloneState: the kinds each repo's
// index has, as far as the records are whole.
func recordKinds(out []byte) map[int]map[string]bool {
	kinds := map[int]map[string]bool{}
	for len(out) > 0 {
		line, rest, ok := bytes.Cut(out, []byte("\n"))
		f := strings.Fields(string(line))
		if !ok || len(f) != 3 {
			return kinds
		}
		i, err1 := strconv.Atoi(f[1])
		size, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || size < 0 || size > len(rest) {
			return kinds
		}
		if kinds[i] == nil {
			kinds[i] = map[string]bool{}
		}
		kinds[i][f[0]] = true
		out = rest[size:]
	}
	return kinds
}

// FuzzParseInit: the VM's answer to its repos' setup names a seed as landed, or of another
// format, only when a line names that seed's path as it was asked; and an old git is named only
// by what git --version prints, else as unknown.
func FuzzParseInit(f *testing.F) {
	for _, s := range []string{
		"landed .\nformat a b\n", "landed elsewhere\nformat ../x\nlanded\n", "oldgit git version 2.25.1 (Apple Git-99)\n",
		"oldgit \x1b[31mred\n", "landed a b\r\nlanded .\n",
	} {
		f.Add(s)
	}
	seeds := []repoSeed{{path: "."}, {path: "a b"}}
	f.Fuzz(func(t *testing.T, out string) {
		a := parseInit(out, seeds)
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimSuffix(l, "\r") // a line may end with CRLF
		}
		for kind, paths := range map[string][]string{"landed": a.landed, "format": a.format} {
			for _, p := range paths {
				if !slices.ContainsFunc(seeds, func(s repoSeed) bool { return s.path == p }) || !slices.Contains(lines, kind+" "+p) {
					t.Fatalf("%s %q, from %q", kind, p, out)
				}
			}
		}
		if a.oldGit != "" && a.oldGit != "unknown" && (!gitVersion.MatchString(a.oldGit) || !slices.Contains(lines, "oldgit "+a.oldGit)) {
			t.Fatalf("an old git %q, from %q", a.oldGit, out)
		}
	})
}

// FuzzParseKept: what a seed's apply kept, which is only shown, is the VM's own lines and names as
// it wrote them: each branch from a kept-ref line, without refs/heads/, and each file and each of
// pre's a name of the NUL-separated list after kept-files, none empty and none holding a NUL; pre
// only after applied-tree.
func FuzzParseKept(f *testing.F) {
	for _, s := range []string{
		"kept-ref refs/heads/agent/x\nkept-ref refs/remotes/origin/main\nkept-ref\nother line\nkept-head\nkept-files\na\tb\x00line\nbreak\x00\x00dir/f\x00",
		"applied-tree\nkept-files\na\x00b\x00\x00p\x00", "kept-files\nkept-ref x\x00kept-head\x00", "lfs-setup-failed\nkept-files\nkept-files\n\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, out string) {
		k := parseKept(out)
		head, names, _ := strings.Cut("\n"+out, "\nkept-files\n")
		lines := strings.Split(head, "\n")
		for _, r := range k.refs {
			if !slices.Contains(lines, "kept-ref "+r) && !slices.Contains(lines, "kept-ref refs/heads/"+r) {
				t.Fatalf("a branch %q, from %q", r, out)
			}
		}
		list := strings.Split(names, "\x00")
		for _, n := range slices.Concat(k.files, k.pre) {
			if n == "" || strings.Contains(n, "\x00") || !slices.Contains(list, n) {
				t.Fatalf("a file %q, from %q", n, out)
			}
		}
		if len(k.pre) > 0 && !k.tree {
			t.Fatalf("pre's files without applied-tree: %q", out)
		}
	})
}

// FuzzParseLFSPointer: a blob is taken as a Git LFS pointer only as the spec writes one, its
// version line first and each line ended, with one oid, 64 lowercase hex digits, which names its
// object's path in .git/lfs/objects, and one size, a decimal of at least 0.
func FuzzParseLFSPointer(f *testing.F) {
	const v = "version https://git-lfs.github.com/spec/v1\n"
	oid := strings.Repeat("ab", 32)
	for _, s := range []string{
		v + "oid sha256:" + oid + "\nsize 12345\n", v + "ext-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n",
		v + "oid sha256:" + oid + "\nsize -1\n", v + "oid sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n", v + "oid sha256:../" + oid[3:] + "\nsize 1\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		o, ok := parseLFSPointer(b)
		if !ok {
			return
		}
		s := string(b)
		lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
		oids, sizes := 0, 0
		for _, l := range lines[1:] {
			oids += strings.Count(l+" ", "oid sha256:"+o.oid+" ") * boolInt(strings.HasPrefix(l, "oid "))
			if n, err := strconv.ParseInt(strings.TrimPrefix(l, "size "), 10, 64); strings.HasPrefix(l, "size ") && err == nil && n == o.size {
				sizes++ // as git-lfs reads it: "+1", "007" and "-0" too
			}
		}
		switch {
		case !strings.HasPrefix(s, v) || !strings.HasSuffix(s, "\n"):
			t.Fatalf("took %q", s)
		case len(o.oid) != 64 || strings.Trim(o.oid, "0123456789abcdef") != "" || path.Base(o.oid) != o.oid:
			t.Fatalf("took the oid %q", o.oid)
		case o.size < 0 || oids != 1 || sizes != 1:
			t.Fatalf("took %q as %+v", s, o)
		}
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
