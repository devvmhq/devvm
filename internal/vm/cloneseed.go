package vm

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/vmname"
)

// The seed: how a --clone VM's clone of a repo gets the repo's commits.
// No git transport carries it, so no receive-pack policy (a shallow
// repo's, a SHA-256 repo's) can refuse it. In the VM, one script makes
// every repo (initRepos); then, for each repo not yet seeded, the host
// sends two parts through the backend's Exec: a small tar of named
// entries (the refs, HEAD, the shallow file), unpacked under the repo's
// git dir, and a pack of the objects the refs reach, made by git
// pack-objects and read by git index-pack as it's sent, never held whole
// on either side. The VM's script then sets the refs and checks out the
// branch, and marks the repo seeded last, so a seed that fails part-way
// runs again in full at the next launch.

// seeder makes a --clone VM's clone, a folder's own repo's (seedClone) or
// a workspace clone's repos' (seedWorkspace, seedRepo), and copies a
// workspace's own files (copyFiles): what clones' other methods, which
// check, fetch and remove what the clone has, build on.
type seeder struct{ clones }

// seedMarker is the file in a VM's repo's git dir that says how far its
// seed got: seedPending from the repo's init, then seedDone once applied.
// A repo with none and a commit was seeded by devvm 0.1.0 or 0.1.1-rc.2's
// push.
const seedMarker = "devvm-seeded"

// seedTreeRecord is the file in a VM's repo's git dir that records what
// its seed put at each path of the repo's uncommitted changes
// (seedTree.record), which the clone check reads back (cloneState).
const seedTreeRecord = "devvm-seeded-tree"

// seedStashRecord is the file in a VM's repo's git dir that lists the
// stash entries its seed stored, one commit a line, which the clone check
// leaves out of the VM's own (cloneState).
const seedStashRecord = "devvm-seeded-stash"

// seedConfigMarker is the file in a VM's repo's git dir that says its
// seed set its settings and hooks (seedConfig), so a try again doesn't
// set them over what the VM changed since.
const seedConfigMarker = "devvm-seeded-config"

// initRepos is the script that makes a --clone VM's repos, all in one
// command: the project folder ($1), the user's; whether ($2) the folder's
// own repo's info/exclude gets what's on stdin; then for each repo its
// path relative to it, the host repo's object format, and remake when the
// user agreed to make its empty repo again. A repo there already that's
// seeded (seedMarker done, or none and a commit) is left as it is, and
// printed as landed. One of another object format with no commit is
// printed as format, or with remake made again; a git before 2.29 has
// only SHA-1, and its rev-parse prints --show-object-format back, so
// any answer but sha1 or sha256 is sha1. Otherwise it's made, unless
// it's there (git 2.54's git init of a reftable repo drops its
// extensions.refStorage, which breaks it), taking pushes to its
// checked-out branch and shallow updates, as the user's own git push
// devvm BRANCH needs, and marked pending. --object-format is passed only
// for SHA-256, which gits older than 2.29 don't know: one that fails
// prints oldgit and its git --version, and stops. Its git runs no hook
// (core.hooksPath=/dev/null), as git init can run reference-transaction.
const initRepos = `ws=$1; ex=$2; shift 2
git() { command git -c core.hooksPath=/dev/null "$@"; }
sudo mkdir -p -- "$ws" && sudo chown -- "$(id -u):$(id -g)" "$ws" || exit 1
while [ "$#" -ge 3 ]; do
  p=$1 fmt=$2 how=$3; shift 3
  d=$ws/$p; g=$ws/$p/.git
  if [ -e "$g" ]; then
    m=$(cat -- "$g/` + seedMarker + `" 2>/dev/null) || m=
    if [ "$m" = 'done' ] || { [ -z "$m" ] && git -C "$d" rev-parse -q --verify HEAD >/dev/null 2>&1; }; then
      printf 'landed %s\n' "$p"; continue
    fi
    have=$(git -C "$d" rev-parse --show-object-format 2>/dev/null) || have=sha1
    case $have in sha1|sha256) ;; *) have=sha1 ;; esac
    if [ "$have" != "$fmt" ]; then
      if [ "$how" != remake ] || git -C "$d" rev-parse -q --verify HEAD >/dev/null 2>&1 || [ -n "$(git -C "$d" for-each-ref 2>/dev/null)" ]; then
        printf 'format %s\n' "$p"; continue
      fi
      rm -rf -- "$g" || exit 1
    fi
  fi
  mkdir -p -- "$d" || exit 1
  if [ -e "$g" ]; then
    :
  elif [ "$fmt" = sha256 ]; then
    git -C "$d" init -q --object-format=sha256 || { printf 'oldgit %s\n' "$(git --version | head -n 1)"; exit 3; }
  else
    git -C "$d" init -q || exit 1
  fi
  git -C "$d" config receive.denyCurrentBranch updateInstead && git -C "$d" config receive.shallowUpdate true || exit 1
  [ -e "$g/` + seedMarker + `" ] || echo pending > "$g/` + seedMarker + `" || exit 1
done
if [ "$ex" = exclude ]; then
  mkdir -p -- "$ws/.git/info" && cat >> "$ws/.git/info/exclude" || exit 1
fi`

// seedStage is the script that unpacks a seed's tar, on stdin, into the
// repo at $1's git dir (devvm-seed), never /tmp, which can be in memory,
// once its init marked it.
const seedStage = `s=$1/.git/devvm-seed
[ -f "$1/.git/` + seedMarker + `" ] || exit 1
rm -rf -- "$s" && mkdir -- "$s" && exec tar -x -f - --no-same-owner -C "$s"`

// seedApply is the script that applies a staged seed (seedStage) to the
// repo at $1, the pack on stdin: git index-pack reads it into the repo
// (and checks every object), a promisor pack for a partial clone's
// (partial), whose missing objects the VM's git fetches from the remote
// the settings name as their promisor, then the shallow file, the refs,
// HEAD and the checkout, then the settings, then the stage goes and the
// repo is marked done, last. Its git runs no hook
// (core.hooksPath=/dev/null), the VM's own or one the seed brings, so
// none can fail a try or a try again.
// It never moves a ref the VM moved: a ref is set when it's missing, at
// the host's value already, or at a value an earlier try of this seed set
// (devvm-seeded-refs); any other is left and printed (kept-ref). A symref
// (symrefs: "NAME TARGET" lines, such as origin/HEAD) is set only where
// NAME isn't a ref already. Each stash entry (stash: "SHA MESSAGE" lines,
// oldest first) is stored once, and recorded (seedStashRecord), so a try
// again skips as many as the record has, and an entry the stash holds
// twice is stored twice. HEAD, a branch's full name or, detached, a
// commit, is set only when HEAD was unborn, it's on the branch the seed
// just set, or it's detached at the commit (or at one an earlier try
// set), so a VM that checked out a branch of its own keeps it, and one
// that moved its detached HEAD keeps it too. A HEAD left where the VM put
// it, which isn't where the host has it, is printed (kept-head), once
// the branch it's on, if the VM moved that, has been (kept-ref). With
// HEAD set, the index is the host's (index, from seedTree), the paths of
// changed.list and gone.list (each ending in a NUL) that the VM has a
// file at already are noted (pre), the working copies the host sent (the
// copies under changed/) are put where the VM has nothing, the record
// (tree.rec) goes in the git dir (seedTreeRecord), so do the state files
// (state/, seedState) and the pseudo-refs and the operation's refs
// (pseudo: "SHA NAME" lines, set with update-ref), applied-tree is
// printed, and the paths checkout lists are checked out from the index,
// those of skip.list (skip-worktree here) with the bit off while they
// are, so checkout-index writes them;
// without the host's index, the index is HEAD's (git reset, which keeps
// the working tree), and every path is checked out. The Git LFS objects
// (lfs/<oid>, writeLFS) go into the git dir's lfs/objects where it has
// none, before anything else, and where the VM has git-lfs, its filter is
// set in this repo's config alone (git lfs install --local --skip-repo:
// no hooks), never the VM's, so a VM that mounts its folder keeps the
// pointers as git stores them, and a failure prints lfs-setup-failed;
// the checkout skips git-lfs's smudge
// (GIT_LFS_SKIP_SMUDGE=1, so it never fetches), and git lfs checkout,
// where the VM has git-lfs, then writes the files whose object is there.
// checkout-index without -f writes only the files that aren't there, so the VM's own
// files stay, and those that differ from the index are printed, after a
// kept-files line, each NUL-terminated; then, after applied-tree, an
// empty one, and pre's, to the end. The settings (config: key, value,
// each ending in a NUL; config.keys, each key, unset first; the hooks,
// hooks/NAME, into the git dir's hooks, only where the VM has none) go
// in once, last, with seedConfigMarker written once they're all in, so a
// try again sets them again only when the first didn't finish. What the
// copy set the refs to (devvm-seeded-refs) is kept as devvm-seed-tips,
// where the walk for the VM's own LFS objects stops (vmLFS.only).
const seedApply = `d=$1; g=$1/.git; s=$1/.git/devvm-seed
git() { command git -c core.hooksPath=/dev/null "$@"; }
[ -f "$s/HEAD" ] && [ -f "$s/refs" ] || exit 1
unborn=; git -C "$d" rev-parse -q --verify HEAD >/dev/null 2>&1 || unborn=1
if [ -f "$s/partial" ]; then
  git -C "$d" index-pack --stdin --promisor >/dev/null || exit 1
else
  git -C "$d" index-pack --stdin >/dev/null || exit 1
fi
if [ -d "$s/lfs" ]; then
  for f in "$s/lfs"/*; do
    [ -f "$f" ] || continue
    o=${f##*/}; a=$(printf %s "$o" | cut -c1-2); b=$(printf %s "$o" | cut -c3-4)
    mkdir -p -- "$g/lfs/objects/$a/$b" || exit 1
    [ -e "$g/lfs/objects/$a/$b/$o" ] || mv -- "$f" "$g/lfs/objects/$a/$b/$o" || exit 1
  done
fi
if command -v git-lfs >/dev/null 2>&1; then git -C "$d" lfs install --local --skip-repo >/dev/null 2>&1 || printf 'lfs-setup-failed\n'; fi
if [ -f "$s/shallow" ]; then cp -- "$s/shallow" "$g/shallow" || exit 1; fi
: >> "$g/devvm-seeded-refs" && : > "$s/update" || exit 1
while read -r sha ref; do
  cur=$(git -C "$d" rev-parse -q --verify "$ref") || cur=
  if [ -z "$cur" ] || [ "$cur" = "$sha" ] || grep -qxF -- "$cur $ref" "$g/devvm-seeded-refs"; then
    printf '%s %s\n' "$sha" "$ref" >> "$g/devvm-seeded-refs" && printf 'update %s %s\n' "$ref" "$sha" >> "$s/update" || exit 1
  else
    printf 'kept-ref %s\n' "$ref"
  fi
done < "$s/refs"
git -C "$d" update-ref --stdin < "$s/update" || exit 1
if [ -f "$s/symrefs" ]; then
  while read -r name target; do
    git -C "$d" rev-parse -q --verify "$name" >/dev/null 2>&1 || git -C "$d" symbolic-ref "$name" "$target" || exit 1
  done < "$s/symrefs"
fi
if [ -f "$s/stash" ]; then
  : >> "$g/` + seedStashRecord + `" || exit 1
  stored=$(wc -l < "$g/` + seedStashRecord + `") && n=0 || exit 1
  while read -r sha m; do
    n=$((n+1)); if [ "$n" -le "$stored" ]; then continue; fi
    git -C "$d" stash store -m "$m" "$sha" && printf '%s\n' "$sha" >> "$g/` + seedStashRecord + `" || exit 1
  done < "$s/stash"
fi
head=$(cat -- "$s/HEAD"); on=$(git -C "$d" symbolic-ref -q HEAD) || on=
set=
case $head in
refs/heads/*)
  if [ -n "$unborn" ] || { [ "$on" = "$head" ] && grep -qF -- "update $head " "$s/update"; }; then
    git -C "$d" symbolic-ref HEAD "$head" || exit 1
    set=1
  else
    [ "$on" = "$head" ] || printf 'kept-head\n'
  fi ;;
*)
  cur=$(git -C "$d" rev-parse -q --verify HEAD) || cur=
  if [ -n "$unborn" ] || { [ -z "$on" ] && { [ "$cur" = "$head" ] || grep -qxF -- "$cur HEAD" "$g/devvm-seeded-refs"; }; }; then
    printf '%s HEAD\n' "$head" >> "$g/devvm-seeded-refs" && git -C "$d" update-ref --no-deref HEAD "$head" || exit 1
    set=1
  else
    printf 'kept-head\n'
  fi ;;
esac
list=
if [ -n "$set" ] && [ -f "$s/index" ]; then
  cp -- "$s/index" "$g/index" || exit 1
  cat -- "$s/changed.list" "$s/gone.list" | xargs -0 sh -c 'd=$1; shift
    for p; do
      if [ -e "$d/$p" ] || [ -L "$d/$p" ]; then printf "%s\000" "$p"; fi
    done' sh "$d" > "$s/pre" || exit 1
  xargs -0 sh -c 'd=$1 s=$2; shift 2
    for p; do
      if [ -e "$d/$p" ] || [ -L "$d/$p" ]; then continue; fi
      mkdir -p -- "$(dirname -- "$d/$p")" && cp -P -p -- "$s/changed/$p" "$d/$p" || exit 1
    done' sh "$d" "$s" < "$s/changed.list" || exit 1
  if [ -f "$s/tree.rec" ]; then cp -- "$s/tree.rec" "$g/` + seedTreeRecord + `" || exit 1; fi
  if [ -s "$s/skip.list" ]; then git -C "$d" update-index --no-skip-worktree -z --stdin < "$s/skip.list" || exit 1; fi
  if [ -d "$s/state" ]; then cp -R -- "$s/state/." "$g/" || exit 1; fi
  if [ -f "$s/exclude" ] && ! grep -qxF '# devvm: from info/exclude on the host' "$g/info/exclude" 2>/dev/null; then
    mkdir -p -- "$g/info" && { echo '# devvm: from info/exclude on the host'; cat -- "$s/exclude"; } >> "$g/info/exclude" || exit 1
  fi
  if [ -f "$s/pseudo" ]; then
    while read -r sha name; do git -C "$d" update-ref --no-deref "$name" "$sha" || exit 1; done < "$s/pseudo"
  fi
  printf 'applied-tree\n'
  list=1
elif [ -n "$set" ]; then
  git -C "$d" reset -q >/dev/null || exit 1
elif [ ! -f "$g/index" ]; then
  git -C "$d" read-tree HEAD || exit 1
fi
if [ -n "$list" ]; then
  LC_ALL=C GIT_LFS_SKIP_SMUDGE=1 git -C "$d" checkout-index -u -z --stdin < "$s/checkout" 2>"$s/checkout.err"
else
  LC_ALL=C GIT_LFS_SKIP_SMUDGE=1 git -C "$d" checkout-index -a -u 2>"$s/checkout.err"
fi || if grep -v -e ' already exists, no checkout$' "$s/checkout.err" >&2; then exit 1; fi
if [ -d "$s/lfs" ] && command -v git-lfs >/dev/null 2>&1; then
  GIT_LFS_SKIP_SMUDGE=0 git -C "$d" lfs checkout >/dev/null 2>&1 || true
fi
if [ -n "$list" ] && [ -s "$s/skip.list" ]; then git -C "$d" update-index --skip-worktree -z --stdin < "$s/skip.list" || exit 1; fi
git -C "$d" update-index -q --refresh >/dev/null 2>&1
echo kept-files; git -C "$d" diff-files --name-only -z
if [ -n "$list" ]; then printf '\000'; cat -- "$s/pre"; fi
if [ -f "$s/config" ] && [ ! -f "$g/` + seedConfigMarker + `" ]; then
  if [ -s "$s/config.keys" ]; then xargs -0 -n 1 git -c core.hooksPath=/dev/null -C "$d" config --unset-all < "$s/config.keys" >/dev/null 2>&1; fi
  if [ -s "$s/config" ]; then xargs -0 -n 2 git -c core.hooksPath=/dev/null -C "$d" config --add < "$s/config" || exit 1; fi
  if [ -d "$s/hooks" ]; then
    mkdir -p -- "$g/hooks" || exit 1
    for f in "$s/hooks"/*; do
      if [ -e "$g/hooks/${f##*/}" ] || [ -L "$g/hooks/${f##*/}" ]; then continue; fi
      cp -p -- "$f" "$g/hooks/${f##*/}" || exit 1
    done
  fi
  : > "$g/` + seedConfigMarker + `" || exit 1
fi
{ [ ! -f "$g/devvm-seeded-refs" ] || cp -- "$g/devvm-seeded-refs" "$g/devvm-seed-tips"; } &&
rm -rf -- "$s" "$g/devvm-seeded-refs" && echo 'done' > "$g/` + seedMarker + `"`

// shallowMax is the most of a repo's shallow file the seed sends: a line
// of at most 65 bytes for each shallow commit.
const shallowMax = 16 << 20

// refPair is a ref the seed sends: src, the host repo's ref, and dst, its
// name in the VM's clone.
type refPair struct{ src, dst string }

// seedRef is a ref the seed sets in the VM's clone: its name there, and
// its object (a commit, or a tag's own object).
type seedRef struct{ sha, name string }

// seedSymref is a symbolic ref the seed sets in the VM's clone, such as
// refs/remotes/origin/HEAD: its name, and the ref it points to.
type seedSymref struct{ name, target string }

// seedData is what one repo's seed sends: its refs and symrefs, what the
// clone checks out (head: a branch's full name, or for a detached HEAD
// the commit), the repo's shallow file, nil when it isn't shallow, and
// its working tree (seedTree), nil when it has no index, its state
// (seedState: an operation in progress, the stash) and its settings
// (seedConfig: the config's allowed keys, the hooks).
type seedData struct {
	refs    []seedRef
	symrefs []seedSymref
	head    string
	shallow []byte
	tree    *seedTree
	state   seedState
	config  seedConfig
	// partial: the repo is a partial clone (partialClone), whose pack is
	// made from a list of the objects it has (packCmd) and is a promisor
	// pack in the VM (the tar's partial); the refs whose object it lacks,
	// left out (readSeed), and its promisor remotes whose settings don't
	// go (keptPromisors), both named.
	partial       bool
	refsMissing   []string
	promisorsKept []string
	// lfs: the Git LFS objects the index's pointers name that the repo
	// has (readLFS).
	lfs lfsSeed
}

// seedNamespaces are the namespaces of the refs a seed sends: branches,
// tags, notes, replace refs and remote-tracking branches. Nothing else
// goes, such as refs/original (filter-branch's backups, which often hold
// what was scrubbed), refs/prefetch or refs/stash.
func seedNamespaces() []string {
	return []string{"refs/heads", "refs/tags", "refs/notes", "refs/replace", "refs/remotes"}
}

// objectFormat is the object format of the repo at dir: sha1 or sha256,
// the two git has.
func objectFormat(ctx context.Context, dir string) (string, error) {
	f, err := hostGit(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil || f != "sha1" && f != "sha256" {
		return "", errors.New("unknown object format " + f)
	}
	return f, nil
}

// headOf is what the repo at dir has checked out: a branch's full name,
// or "" for a detached HEAD.
func headOf(ctx context.Context, dir string) string {
	if ref, err := hostGit(ctx, dir, "symbolic-ref", "-q", "HEAD"); err == nil && strings.HasPrefix(ref, "refs/heads/") {
		return ref
	}
	return ""
}

// seedTips are the refs the seed of the repo at dir sends, head its
// branch checked out ("" for a detached HEAD), each at its own name. A
// repo (a folder's own, or a workspace's) sends every ref of
// seedNamespaces. A linked worktree, whose repo is elsewhere and shared
// with other worktrees, sends what's published and its own work: its
// branch and that branch's upstream, if it has one and it's one ref (the
// repo's config can make it a pattern, which would send every branch),
// the repo's default branch (defaultBranchOf), every remote-tracking
// branch and every tag, and not the repo's other local branches, which
// are other worktrees' work. Neither sends a devvm VM's remote-tracking
// branches (vmRemotes).
func seedTips(ctx context.Context, dir, head string, worktree bool) ([]refPair, error) {
	out, err := hostGit(ctx, dir, append([]string{"for-each-ref", "--format=%(refname)"}, seedNamespaces()...)...)
	if err != nil {
		return nil, err
	}
	vms := vmRemotes(ctx, dir)
	var refs []refPair
	seen := map[string]bool{}
	// Every pair, a worktree's upstream and default branch included, comes
	// from the namespaces, never from another VM's remote.
	add := func(src, dst string) {
		inSpace := slices.ContainsFunc(seedNamespaces(), func(n string) bool { return strings.HasPrefix(src, n+"/") })
		fromVM := slices.ContainsFunc(vms, func(p string) bool { return strings.HasPrefix(src, p) })
		if inSpace && !fromVM && !seen[dst] {
			seen[dst] = true
			refs = append(refs, refPair{src, dst})
		}
	}
	if worktree && head != "" {
		add(head, head)
		if up, err := hostGit(ctx, dir, "for-each-ref", "--format=%(upstream)", head); err == nil && strings.HasPrefix(up, "refs/") && up != head && validRef(ctx, dir, up) {
			add(up, up)
		}
	}
	if def, from := defaultBranchOf(ctx, dir); worktree && def != "" {
		add(from, "refs/heads/"+def)
	}
	for _, r := range strings.Fields(out) {
		if published := strings.HasPrefix(r, "refs/remotes/") || strings.HasPrefix(r, "refs/tags/"); worktree && !published {
			continue
		}
		add(r, r)
	}
	return refs, nil
}

// vmRemotes are the remote-tracking namespaces ("refs/remotes/NAME/") of
// the repo at dir's remotes any of whose URLs, as git resolves them
// (remote get-url --all, insteadOf applied), reaches a devvm VM (vmHost):
// another VM's work, which no seed sends.
func vmRemotes(ctx context.Context, dir string) []string {
	out, _ := hostGit(ctx, dir, "remote")
	var vms []string
	for _, name := range strings.Fields(out) {
		urls, _ := hostGit(ctx, dir, "remote", "get-url", "--all", name)
		if slices.ContainsFunc(strings.Fields(urls), func(u string) bool { return vmHost(urlHost(u)) }) {
			vms = append(vms, "refs/remotes/"+name+"/")
		}
	}
	return vms
}

// urlHost is the host a git URL names, lower case: an ssh://,
// git+ssh:// or ssh+git:// URL's, or a scp-like [user@]host:path's; ""
// for a path or another URL.
func urlHost(u string) string {
	scheme, _, ok := strings.Cut(u, "://")
	if !ok {
		host, _, _ := scpHost(u)
		return strings.ToLower(host)
	}
	switch strings.ToLower(scheme) {
	case "ssh", "git+ssh", "ssh+git":
	default:
		return ""
	}
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(p.Hostname())
}

// vmHost reports whether host is a devvm VM's name in ssh: a project VM's
// full name (vmname.IsVM), or its shorter one (vmname.Short:
// devvm-<folder>), which is a full name without its hash.
func vmHost(host string) bool {
	return vmname.IsVM(host) || strings.HasPrefix(host, vmname.Prefix) && vmname.IsVM(host+"-"+strings.Repeat("0", vmname.HashLen))
}

// hostRef is a ref of the host's repo as readSeed reads it: its object,
// that object's type, and for a symbolic ref, the ref it points to.
type hostRef struct{ sha, kind, symref string }

// hostRefs is every ref of seedNamespaces in the repo at dir, read at
// once, each object's type read after (git cat-file), "missing" for one
// the repo lacks, such as a partial clone's tag of a blob it never
// fetched, which git for-each-ref's %(objecttype) would fail on.
func hostRefs(ctx context.Context, dir string) (map[string]hostRef, error) {
	out, err := hostGit(ctx, dir, append([]string{"for-each-ref", "--format=%(objectname) %(refname) %(symref)"}, seedNamespaces()...)...)
	if err != nil {
		return nil, err
	}
	refs := map[string]hostRef{}
	var ids strings.Builder
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 || !objectName.MatchString(f[0]) {
			continue
		}
		r := hostRef{sha: f[0], kind: "missing"}
		if len(f) > 2 {
			r.symref = f[2]
		}
		refs[f[1]] = r
		ids.WriteString(f[0] + "\n")
	}
	cmd := hostgit.Command(ctx, "-C", dir, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	cmd.Stdin = strings.NewReader(ids.String())
	types, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{}
	for line := range strings.Lines(string(types)) {
		if f := strings.Fields(line); len(f) == 2 && objectName.MatchString(f[0]) {
			kinds[f[0]] = f[1]
		}
	}
	for name, r := range refs {
		if k, ok := kinds[r.sha]; ok {
			r.kind = k
			refs[name] = r
		}
	}
	return refs, nil
}

// readSeed reads what the seed of the repo at dir sends: refs' objects,
// as they are now (a tag's own object, so an annotated tag stays one),
// a symbolic ref as one, when the ref it points to goes too, head (a
// branch's full name, else the commit HEAD is detached at), and its
// shallow file, read from the repo's common dir (a linked worktree's is
// its repo's) through safefile, so a link there can't send another file.
// A branch that's no commit, which git refuses to set, is left out, and
// the branch to check out must be one.
func readSeed(ctx context.Context, dir, head string, refs []refPair) (seedData, error) {
	all, err := hostRefs(ctx, dir)
	if err != nil {
		return seedData{}, err
	}
	s := seedData{head: head}
	var symrefs []seedSymref
	for _, p := range refs {
		r, ok := all[p.src]
		switch {
		case !ok:
		case r.symref != "" && p.src == p.dst:
			symrefs = append(symrefs, seedSymref{name: p.dst, target: r.symref})
		case r.kind == "missing":
			s.refsMissing = append(s.refsMissing, p.dst)
		case strings.HasPrefix(p.dst, "refs/heads/") && r.kind != "commit":
		default:
			s.refs = append(s.refs, seedRef{sha: r.sha, name: p.dst})
		}
	}
	// A symref goes only when the ref it points to does.
	for _, sr := range symrefs {
		if slices.ContainsFunc(s.refs, func(r seedRef) bool { return r.name == sr.target }) {
			s.symrefs = append(s.symrefs, sr)
		}
	}
	if head == "" {
		sha, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}")
		if err != nil || !objectName.MatchString(sha) {
			return seedData{}, errors.New("HEAD is no commit")
		}
		s.head = sha
	} else if !slices.ContainsFunc(s.refs, func(r seedRef) bool { return r.name == head }) {
		return seedData{}, errors.New("no " + head + " to check out")
	}
	_, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return seedData{}, err
	}
	shallow, err := safefile.Read(filepath.Join(common, "shallow"), shallowMax, safefile.Opts{})
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return seedData{}, err
	default:
		s.shallow = shallow
	}
	return s, nil
}

// readSeedState is readSeed of s, with its state (readRepoState) and its
// settings (readConfig).
func readSeedState(ctx context.Context, s repoSeed) (seedData, error) {
	data, err := readSeed(ctx, s.dir, s.head, s.refs)
	if err != nil {
		return seedData{}, err
	}
	// Only a partial clone lacks objects of its own refs; any other repo
	// that does is broken.
	if data.partial = partialClone(ctx, s.dir); !data.partial && len(data.refsMissing) > 0 {
		return seedData{}, msg.CloneRefNoObject(data.refsMissing[0])
	}
	if data.state, err = readRepoState(ctx, s.dir, s.worktree); err != nil {
		return seedData{}, err
	}
	if data.config, err = readConfig(ctx, s.dir, data.refs); err != nil {
		return seedData{}, err
	}
	if data.partial {
		data.promisorsKept = data.keptPromisors(ctx, s.dir)
	}
	return data, nil
}

// todoMissing are the commits the state's todo lists name that s's pack
// won't carry (missingCommits): its refs and head reach none of them.
func (s seedData) todoMissing(ctx context.Context, dir string) []string {
	if len(s.state.todo) == 0 {
		return nil
	}
	var tips strings.Builder
	for _, r := range s.refs {
		tips.WriteString(r.sha + "\n")
	}
	if objectName.MatchString(s.head) {
		tips.WriteString(s.head + "\n")
	}
	for _, r := range slices.Concat(s.state.pseudo, s.state.refs) {
		if r.name != "AUTO_MERGE" { // a tree, which rev-list refuses
			tips.WriteString(r.sha + "\n")
		}
	}
	tips.WriteString(strings.Join(s.state.tips, "\n"))
	return missingCommits(ctx, dir, s.state.todo, tips.String())
}

// tips are the objects s's pack is made of, its refs', a detached HEAD's,
// its index's blobs and its state's (seedState.objects), one a line, each
// once.
func (s seedData) tips() string {
	var b strings.Builder
	seen := map[string]bool{}
	shas := make([]string, 0, len(s.refs)+1)
	for _, r := range s.refs {
		shas = append(shas, r.sha)
	}
	if objectName.MatchString(s.head) {
		shas = append(shas, s.head)
	}
	if s.tree != nil {
		shas = append(shas, s.tree.blobs...)
	}
	shas = append(shas, s.state.objects()...)
	for _, sha := range shas {
		if !seen[sha] {
			seen[sha] = true
			b.WriteString(sha + "\n")
		}
	}
	return b.String()
}

// tarEntry is one file of a seed's tar.
type tarEntry struct {
	name string
	data []byte
}

// writeTar writes s's tar to w: refs ("SHA NAME" lines), HEAD (a branch's
// full name, or a commit), and when there are any, symrefs ("NAME TARGET"
// lines), for a shallow repo, shallow, the state's entries
// (seedState.entries), the settings' (seedConfig.entries, each with its
// mode) and the working tree's (seedTree.write).
func (s seedData) writeTar(w io.Writer) error {
	var refs, symrefs strings.Builder
	for _, r := range s.refs {
		refs.WriteString(r.sha + " " + r.name + "\n")
	}
	for _, r := range s.symrefs {
		symrefs.WriteString(r.name + " " + r.target + "\n")
	}
	tw := tar.NewWriter(w)
	entries := []tarEntry{{"refs", []byte(refs.String())}, {"HEAD", []byte(s.head + "\n")}}
	if symrefs.Len() > 0 {
		entries = append(entries, tarEntry{"symrefs", []byte(symrefs.String())})
	}
	if s.shallow != nil {
		entries = append(entries, tarEntry{"shallow", s.shallow})
	}
	if s.partial {
		entries = append(entries, tarEntry{"partial", nil})
	}
	for _, e := range append(entries, s.state.entries()...) {
		if err := writeEntry(tw, e); err != nil {
			return err
		}
	}
	for _, e := range s.config.entries() {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Size: int64(len(e.data)), Mode: e.mode}); err != nil {
			return err
		}
		if _, err := tw.Write(e.data); err != nil {
			return err
		}
	}
	if s.tree != nil {
		if err := s.tree.write(tw); err != nil {
			return err
		}
	}
	if err := writeLFS(tw, s.lfs); err != nil {
		return err
	}
	return tw.Close()
}

// initAnswer is what the VM said to initRepos, of the seeds it was asked
// about only: those seeded already (landed), those whose empty repo has
// another object format (format), and, when its git couldn't make a
// SHA-256 repo, that git's version (oldGit, "" when it could).
type initAnswer struct {
	landed, format []string
	oldGit         string
}

// gitVersion matches what git --version prints, as the VM's answer may
// name it: anything else is shown as unknown.
var gitVersion = regexp.MustCompile(`^git version [0-9A-Za-z.+ ()-]{1,64}$`)

// parseInit reads the VM's answer to initRepos, out, for seeds: a line
// counts only when it names one of them, as the VM wrote it.
func parseInit(out string, seeds []repoSeed) initAnswer {
	var a initAnswer
	s := bufio.NewScanner(strings.NewReader(out))
	for s.Scan() {
		kind, rest, _ := strings.Cut(s.Text(), " ")
		ours := slices.ContainsFunc(seeds, func(r repoSeed) bool { return r.path == rest })
		switch {
		case kind == "landed" && ours:
			a.landed = append(a.landed, rest)
		case kind == "format" && ours:
			a.format = append(a.format, rest)
		case kind == "oldgit":
			a.oldGit = "unknown"
			if gitVersion.MatchString(rest) {
				a.oldGit = rest
			}
		}
	}
	return a
}

// seedKept is what a seed's apply left of the VM's own (seedApply): the
// branches the VM moved, which it didn't move to the host's, and the
// files whose content isn't the branch's, which it didn't write over.
type seedKept struct {
	refs, files []string
	head        bool // HEAD stays where the VM put it
	// The working tree the host sent was applied (applied-tree), and the
	// paths of it the VM had a file at already.
	tree bool
	pre  []string
	// lfs: git-lfs's filter couldn't be set in the VM's repo
	// (lfs-setup-failed).
	lfs bool
}

// parseKept reads seedApply's answer, out: kept-ref, kept-head,
// applied-tree and lfs-setup-failed lines, then, after a kept-files line, paths each ending in
// a NUL, and after an empty one, pre's. What the VM printed is untrusted,
// so it's only shown (shown).
func parseKept(out string) seedKept {
	var k seedKept
	refs, files, _ := strings.Cut("\n"+out, "\nkept-files\n")
	for line := range strings.Lines(refs) {
		line = strings.TrimSuffix(line, "\n")
		if r, ok := strings.CutPrefix(line, "kept-ref "); ok && r != "" {
			k.refs = append(k.refs, strings.TrimPrefix(r, "refs/heads/"))
		}
		k.head = k.head || line == "kept-head"
		k.tree = k.tree || line == "applied-tree"
		k.lfs = k.lfs || line == "lfs-setup-failed"
	}
	into := &k.files
	for f := range strings.SplitSeq(strings.TrimSuffix(files, "\x00"), "\x00") {
		switch {
		case f != "":
			*into = append(*into, f)
		case k.tree:
			into = &k.pre
		}
	}
	return k
}

// shownMax is the most of each name shown shows.
const shownMax = 200

// shown is names, at most cloneShown of them, each cut to shownMax bytes
// and quoted (quoted).
func shown(names []string) string {
	cut := make([]string, 0, cloneShown)
	for _, n := range names[:min(len(names), cloneShown)] {
		if len(n) > shownMax {
			n = strings.ToValidUTF8(n[:shownMax], "") + "…"
		}
		cut = append(cut, n)
	}
	return quoted(cut)
}

// seed copies the repo of s into its clone in t's running VM, which
// initRepos made: it reads what to send (readSeed), then sends the tar
// (seedStage) and the pack (seedApply), showing step meanwhile, and names
// what the VM's own it kept.
func (e seeder) seed(ctx context.Context, t *target, s repoSeed, step msg.M) error {
	data, err := readSeedState(ctx, s)
	if err != nil {
		return msg.CloneSeedFailed(t.f.Name).Because(err)
	}
	tree, err := newSeedTree()
	if err != nil {
		return msg.CloneSeedFailed(t.f.Name).Because(err)
	}
	// Its index's copy goes, however the seed ends.
	done := e.AddCleanup(func(context.Context) { tree.close() })
	defer func() { tree.close(); done() }()
	if ok, err := tree.load(ctx, s.dir); err != nil {
		return msg.CloneSeedFailed(t.f.Name).Because(err)
	} else if ok {
		data.tree = tree
		if data.lfs, err = readLFS(ctx, s.dir, tree.blobs); err != nil {
			return msg.CloneSeedFailed(t.f.Name).Because(err)
		}
		if bad := data.lfs.bad; len(bad) > 0 {
			e.Warn(msg.CloneLfsBad(len(bad), shown(bad), s.path))
		}
		if data.lfs.elsewhere {
			e.Warn(msg.CloneLfsStorage(s.path))
		}
	}
	stop := e.Status(step)
	defer stop()
	if err := e.sendTar(ctx, t, s.dir, data); err != nil {
		return err
	}
	out, err := e.sendPack(ctx, t, s.dir, data)
	if err != nil {
		return err
	}
	stop()
	kept := parseKept(out)
	e.sayState(ctx, s.dir, data, kept.tree)
	e.sayConfig(s.dir, data.config)
	e.sayPartial(t, s.dir, data)
	e.sayKept(s.dir, data.tree, kept)
	if kept.lfs {
		e.Warn(msg.CloneLfsSetupFailed(s.path))
	}
	return nil
}

// sayState names what of the state of the repo at dir its seed (data)
// left on this machine: the stash entries of git stash --all, and those
// past stashMax, and, when the state went in (applied), the commits its
// todo lists name that the VM lacks.
func (e seeder) sayState(ctx context.Context, dir string, data seedData, applied bool) {
	if all := data.state.stashAll; len(all) > 0 {
		e.Warn(msg.CloneStashAllKept(len(all), dir, shown(all)))
	}
	if over := data.state.stashOver; len(over) > 0 {
		e.Warn(msg.CloneStashOverKept(len(over), dir, shown(over), strconv.Itoa(stashMax)))
	}
	if !applied {
		return
	}
	if missing := data.todoMissing(ctx, dir); len(missing) > 0 {
		e.Warn(msg.CloneTodoMissing(shown(missing), len(missing), dir))
	}
}

// sayKept names what the seed of the repo at dir, whose working tree was
// tree (nil for none), kept of the VM's own (kept), and what of the
// repo's own changes it didn't copy: those that a filter changes and
// those that aren't a file any more, or, when the VM's HEAD was kept, all
// of them, an operation in progress included.
func (e seeder) sayKept(dir string, tree *seedTree, kept seedKept) {
	if len(kept.refs) > 0 {
		e.Warn(msg.CloneKeptBranches(shown(kept.refs), len(kept.refs), dir))
	}
	if kept.head {
		e.Warn(msg.CloneKeptHead(dir))
	}
	switch {
	case tree == nil:
	case !kept.tree:
		if len(tree.covered) > 0 {
			e.Warn(msg.CloneUncommittedKept(dir))
		}
	default:
		kept.files = tree.ownFiles(kept.files, kept.pre)
		if len(tree.filtered) > 0 {
			e.Warn(msg.CloneFilteredKept(len(tree.filtered), dir, shown(tree.filtered)))
		}
		if len(tree.typechanged) > 0 {
			e.Warn(msg.CloneTypechangeKept(len(tree.typechanged), dir, shown(tree.typechanged)))
		}
		if len(tree.skipped) > 0 {
			e.Warn(msg.CloneSkipWorktreeKept(len(tree.skipped), dir, shown(tree.skipped)))
		}
	}
	if len(kept.files) > 0 {
		e.Warn(msg.CloneKeptFiles(len(kept.files), dir, shown(kept.files)))
	}
}

// sendTar sends data's tar into the clone at dir in t's VM (seedStage),
// written as the VM reads it.
func (e seeder) sendTar(ctx context.Context, t *target, dir string, data seedData) error {
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(data.writeTar(pw)) }()
	log := e.vmLog()
	err := t.exec(ctx, []string{"sh", "-c", seedStage, "sh", dir}, backend.ExecOpts{Stdin: pr, Stdout: log, Stderr: log})
	_ = pr.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(t.f.Name))
	}
	return nil
}

// packErrMax is the most of git pack-objects' errors the run log gets.
const packErrMax = 64 << 10

// packCmd is the host git command whose output is data's pack, its
// errors into stderr, and for a partial clone the command whose output is
// its input (list), which its caller starts too: git pack-objects --revs
// of the tips, which uses bitmaps, or for a partial clone, whose walk
// would want objects only its remote has, git rev-list --objects
// --missing=allow-promisor of the tips the repo has (haveObjects: a
// sparse checkout's index can name blobs it never fetched), piped into
// git pack-objects, whose pack goes through packTail, which cuts it short
// when rev-list fails. Each command's line goes into log, the run log,
// redacted as LoggedCmd's are: not LoggedCmd, which runs a command to its
// end, while this one's output is the VM command's input, read as it's
// made.
func packCmd(ctx context.Context, dir string, data seedData, stderr, log io.Writer) (cmd *exec.Cmd, list *packList, err error) {
	// Both commands' errors, written at once.
	stderr = &lockedWriter{w: stderr}
	logged := func(ctx context.Context, args ...string) *exec.Cmd {
		_, _ = io.WriteString(log, redact.String("+ git "+strings.Join(args, " "))+"\n")
		c := hostgit.Command(ctx, args...)
		c.Stderr = stderr
		return c
	}
	if !data.partial {
		cmd = logged(ctx, "-C", dir, "pack-objects", "--revs", "--stdout", "-q")
		cmd.Stdin = strings.NewReader(data.tips())
		return cmd, nil, nil
	}
	have, err := haveObjects(ctx, dir, strings.Fields(data.tips()))
	if err != nil {
		return nil, nil, err
	}
	listCtx, cancel := context.WithCancel(ctx)
	rl := logged(listCtx, "-C", dir, "rev-list", "--objects", "--missing=allow-promisor", "--stdin")
	rl.Stdin = strings.NewReader(strings.Join(have, "\n") + "\n")
	cmd = logged(ctx, "-C", dir, "pack-objects", "--stdout", "-q")
	pr, pw, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	rl.Stdout, cmd.Stdin = pw, pr
	return cmd, &packList{cmd: rl, cancel: cancel, pr: pr, pw: pw}, nil
}

// sendPack sends the pack of data's commits into the clone at dir in t's
// VM and applies the seed there (seedApply): git pack-objects' output is
// the command's input, as it's made, and it stops once the command ends,
// as its output then goes nowhere. It returns what the VM printed.
func (e seeder) sendPack(ctx context.Context, t *target, dir string, data seedData) (string, error) {
	vm := t.f.Name
	stderr := &bounded{max: packErrMax}
	cmd, list, err := packCmd(ctx, dir, data, stderr, e.LogWriter())
	var pack io.ReadCloser
	if err == nil {
		pack, err = cmd.StdoutPipe()
	}
	if err == nil && list != nil {
		err = list.cmd.Start()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if pack != nil {
			_ = pack.Close()
		}
		if list != nil {
			_ = list.finish()
		}
		return "", msg.CloneSeedFailed(vm).Because(err)
	}
	var in io.Reader = pack
	if list != nil {
		list.started()
		in = &packTail{r: pack, check: list.wait}
	}
	out := &bounded{max: cloneOutMax}
	err = t.exec(ctx, []string{"sh", "-c", seedApply, "sh", dir}, backend.ExecOpts{Stdin: in, Stdout: out, Stderr: e.vmLog()})
	_ = pack.Close()
	werr := cmd.Wait()
	var lerr error
	if list != nil {
		lerr = list.finish()
	}
	_, _ = io.WriteString(e.LogWriter(), redact.String(stderr.String()))
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case lerr != nil:
		_, _ = io.WriteString(e.LogWriter(), redact.String(out.String()))
		return "", msg.CloneSeedFailed(vm).Because(lerr)
	case werr != nil && err == nil:
		return "", msg.CloneSeedFailed(vm).Because(werr)
	case err != nil:
		_, _ = io.WriteString(e.LogWriter(), redact.String(out.String()))
		return "", backend.Logged(msg.CloneSeedFailed(vm))
	}
	return out.String(), nil
}

// askRemake asks whether to make the empty repos at paths in vm again,
// which have another object format than the host's repo: an earlier
// devvm made them SHA-1 (initRepos). Only the user can judge whether
// what's in them, hooks or settings, can go, so no flag answers it.
func (e seeder) askRemake(vm string, paths []string) error {
	names := quoted(paths)
	yes, err := e.Ask(msg.CloneRepoFormat(names, vm).RefusedWith(msg.CloneRepoFormatKept(names, vm)))
	if err != nil {
		return err
	}
	if !yes {
		return msg.CloneRepoFormatKept(names, vm)
	}
	return nil
}
