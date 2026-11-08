package vm

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// Each agent's settings (agents.Def's Dirs, Files and Folder) are paths
// relative to the home folder, which devvm's store of the agent's
// settings (agentStore) keeps at the same paths below it. Folders are
// mounted read-only at their path in the store and linked into the VM's
// home; files are copied in on every launch. An agent's logins are in the
// store of its logins (secrets.LoginFile). History (transcripts,
// sessions, databases) is in neither.

// agentStore is devvm's store of an agent's settings, laid out as they
// are in the home folder: the one place devvm reads them from for VMs,
// never the home folder.
func agentStore(d paths.Dirs, agent string) string { return d.AgentSettings(agent) }

// opencodeData is opencode's data folder. opencode 2 keeps its sessions
// and its logins in one SQLite database there, opencode.db (or
// opencode-<channel>.db for another channel's build), in WAL mode, so
// the database's -wal file holds the pages it changed last, its logins'
// among them, until opencode checkpoints it.
const opencodeData = agents.OpencodeData

// opencodeDBs are the names of opencode's databases and their WAL,
// shared-memory and rollback journal files in opencodeData, as globs.
// An export and an import never take them as they are: only the
// database's copy that opencodeClean makes.
var opencodeDBs = []string{"opencode*.db", "opencode*.db-wal", "opencode*.db-shm", "opencode*.db-journal"}

// opencodeLogins is the SQL condition that picks the tables of
// opencode's database that hold logins: opencode 2.2's credential
// (logins and API keys), account and control_account (its console's
// access and refresh tokens) and account_state, an earlier opencode's
// session_share (each share's secret), and any other table whose name or
// a column's says it holds one, so a table a later opencode adds for its
// logins is picked too. testdata/opencode-2.2.5.sql is opencode's own
// schema, which TestOpencodeCleanSchema checks it against.
const opencodeLogins = `type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND (` +
	`name LIKE '%credential%' OR name LIKE '%account%' OR name LIKE '%auth%' OR name LIKE '%secret%' ` +
	`OR name LIKE '%token%' OR name LIKE '%share%' OR name IN (SELECT m.name FROM sqlite_master m ` +
	`JOIN pragma_table_info(m.name) c WHERE m.type = 'table' AND lower(c.name) IN ('secret', 'access_token', ` +
	`'refresh_token', 'id_token', 'api_key', 'apikey', 'password', 'token', 'credential', 'credentials')))`

// opencodeAuthImport is opencode 2's migration that imports auth.json
// into its credential table, once. opencodeClean takes its record out,
// so the opencode that next opens the copy imports the logins devvm's
// setup wrote in that VM's auth.json, as it does when it makes its
// database.
const opencodeAuthImport = "20260805200742_import_legacy_credentials"

// opencodeWellknown is the SQL condition that picks the rows of
// opencode's kv table that hold an organization's remote config, which
// opencode fetched with its well-known login's headers, one row for each
// organization; opencode fetches it again after that login.
const opencodeWellknown = `key LIKE 'wellknown:config:%'`

// opencodeClean is the shell function devvm_opencode_clean DB, which
// makes clean.db in the current folder: a copy of opencode's database
// DB without its logins. sqlite3 copies it with VACUUM INTO, which reads
// the database with what its -wal file holds, as one snapshot; in the
// copy it drops every trigger, empties the tables opencodeLogins picks,
// deletes the kv rows opencodeWellknown picks and takes
// opencodeAuthImport's record out, then turns WAL off and rebuilds the
// file (secure_delete and VACUUM), so no page of it keeps a deleted row.
// It fails when the VM has no sqlite3, when any step fails, when the
// copy has fewer tables than DB or none, or unless each count of rows
// left (the triggers, each table it emptied, kv's rows it deleted)
// prints 0, so a sqlite3 that runs nothing and prints nothing fails too;
// the caller then leaves the database out. The schema's own functions stay off
// (trusted_schema), as an imported database is the export's: each
// sqlite3 gets that pragma first in its own input, never in -cmd, as
// sqlite3 3.54 with -bail stops after an SQL -cmd and runs nothing
// else, exiting 0.
func opencodeClean() string {
	// opencodeWellknown in an SQL string literal, its quotes doubled.
	wellknown := strings.ReplaceAll(opencodeWellknown, "'", "''")
	gen := `SELECT printf('DROP TRIGGER IF EXISTS "%w";', name) FROM sqlite_master WHERE type = 'trigger';` + "\n" +
		`SELECT printf('DELETE FROM "%w";', name) FROM sqlite_master WHERE ` + opencodeLogins + ";\n" +
		`SELECT 'DELETE FROM migration WHERE id = ''` + opencodeAuthImport + `'';' FROM sqlite_master ` +
		`WHERE type = 'table' AND name = 'migration';` + "\n" +
		`SELECT 'DELETE FROM kv WHERE ` + wellknown + `;' FROM sqlite_master WHERE type = 'table' AND name = 'kv';`
	check := `SELECT 'SELECT count(*) FROM sqlite_master WHERE type = ''trigger'';';` + "\n" +
		`SELECT printf('SELECT count(*) FROM "%w";', name) FROM sqlite_master WHERE ` + opencodeLogins + ";\n" +
		`SELECT 'SELECT count(*) FROM kv WHERE ` + wellknown + `;' FROM sqlite_master WHERE type = 'table' AND name = 'kv';`
	const untrusted = "PRAGMA trusted_schema = OFF;"
	tables := shell.Quote("SELECT count(*) FROM sqlite_master WHERE type = 'table';")
	// sql runs sqlite3 on clean.db with untrusted first, then its
	// arguments, or its stdin when it has none.
	sql := "sqlite3 -bail clean.db '" + untrusted + "'"
	return strings.Join([]string{
		"devvm_opencode_clean() {",
		"  local t s c left",
		"  rm -f clean.db clean.db-journal clean.db-wal clean.db-shm &&",
		// The copy, then the original's count of tables, which the copy
		// must have too: a copy that isn't made, or is empty, fails.
		`  t=$(sqlite3 -bail "$1" '.timeout 10000' '` + untrusted + `' "VACUUM INTO 'clean.db'" ` + tables + `) || return 1`,
		`  case $t in '' | 0 | *[!0-9]*) return 1 ;; esac`,
		`  [ "$(` + sql + " " + tables + `)" = "$t" ] &&`,
		"  s=$(" + sql + " " + shell.Quote(gen) + ") &&",
		`  printf '` + untrusted + `\nPRAGMA journal_mode = DELETE;\nPRAGMA secure_delete = ON;\n%s\nVACUUM;\n' "$s" | sqlite3 -bail clean.db >/dev/null &&`,
		// check's first statement, the triggers' count, is always there,
		// and each of its statements must print one 0: no output, or
		// less, fails too.
		"  c=$(" + sql + " " + shell.Quote(check) + ") && [ -n \"$c\" ] &&",
		`  left=$(printf '` + untrusted + `\n%s\n' "$c" | sqlite3 -bail clean.db) && [ -n "$left" ] &&`,
		`  [ "$(printf '%s\n' "$left" | grep -cx 0)" = "$(printf '%s\n' "$c" | grep -c '')" ] &&`,
		`  ! printf '%s\n' "$left" | grep -qvx 0 || return 1`,
		"  [ ! -e clean.db-wal ] && [ ! -e clean.db-journal ]",
		"}",
	}, "\n")
}

// opencodeCleanAll is the shell loop that runs devvm_opencode_clean on
// each of opencode's databases in the folder dir (shell-quoted), from the
// current folder, then runs then with $db, the database, and $n, its
// name, for the copy it made. When there's no copy, or then fails, it
// says so and runs otherwise. A database that's a symbolic link is never
// read.
func opencodeCleanAll(dir, then, otherwise string) string {
	return strings.Join([]string{
		"for db in " + dir + "/" + opencodeDBs[0] + "; do",
		`  [ -f "$db" ] && [ ! -L "$db" ] || continue`,
		`  n=${db##*/}`,
		`  if ! command -v sqlite3 >/dev/null; then echo "devvm: left out $db: the VM has no sqlite3" >&2`,
		`  elif devvm_opencode_clean "$db" && ` + then + "; then continue",
		`  else echo "devvm: left out $db: its copy without opencode's logins failed" >&2`,
		"  fi",
		"  " + otherwise,
		"done",
	}, "\n")
}

// historyExport is the script that writes an export of the VM's agents'
// history to its stdout, a gzipped tar of what there is of their History,
// run in the VM as its user: without their HistoryExcludes, and with each
// of opencode's databases as its copy without its logins (opencodeClean),
// made in a folder of its own and put at the database's path
// (--transform, which leaves links' targets as they are), never the
// database or its WAL and journal files as they are (opencodeDBs, each
// matched within its folder).
func historyExport() string {
	historyExcludes := agents.HistoryExcludes()
	excludes := make([]string, 0, len(historyExcludes)+len(opencodeDBs)+1)
	for _, p := range historyExcludes {
		excludes = append(excludes, "--exclude="+p)
	}
	excludes = append(excludes, "--no-wildcards-match-slash")
	for _, p := range opencodeDBs {
		excludes = append(excludes, "--exclude="+opencodeData+"/"+p)
	}
	// No pipefail: ls fails on the paths there aren't, and only tar's
	// status counts.
	return strings.Join([]string{
		opencodeClean(),
		`mkdir -p ~/.cache && t=$(mktemp -d ~/.cache/devvm-export.XXXXXX) || exit 1`,
		`trap 'rm -rf "$t"' EXIT`,
		`mkdir "$t/devvm-clean" && cd "$t" || exit 1`,
		"add=()",
		opencodeCleanAll(`"$HOME"/`+shell.Quote(opencodeData),
			`mv clean.db "devvm-clean/$n" && add=(${add[@]+"${add[@]}"} "devvm-clean/$n")`, ":"),
		`[ "${#add[@]}" -eq 0 ] || add=(-C "$t" "${add[@]}")`,
		"cd ~ && ls -d " + strings.Join(agents.History(), " ") + " 2>/dev/null | tar -czf - " + shell.QuoteJoin(excludes) +
			" " + shell.Quote("--transform=flags=r;s,^devvm-clean/,"+opencodeData+"/,") + ` -T - ${add[@]+"${add[@]}"}`,
	}, "\n")
}

// historyUnsafe is what historyImport prints when it imports nothing
// because a path in the export is outside the agents' history.
const historyUnsafe = "devvm-history-unsafe"

// importSkips are the paths in an export of agents' history, relative to
// the home folder, that an import leaves out, because devvm's setup
// writes them: what an export leaves out (HistoryExcludes), every login
// file in the agents' history, such as ~/.claude.json, whose copy holds
// the old VM's placeholders, and every agent's folders linked to their
// mounts and files copied from the host. Sorted.
func importSkips() []string {
	skips := agents.HistoryExcludes()
	for _, d := range agents.Defs {
		for _, l := range d.Logins {
			if slices.ContainsFunc(d.History, func(h string) bool { return l.Rel == h || strings.HasPrefix(l.Rel, h+"/") }) {
				skips = append(skips, l.Rel)
			}
		}
		skips = append(skips, d.Dirs...)
		for _, f := range d.Files {
			skips = append(skips, f.Rel)
		}
	}
	slices.Sort(skips)
	return slices.Compact(skips)
}

// historyImport is the script that puts an export of agents' history,
// on its stdin, into the VM's home, run there as the VM's user, so the
// host never reads the export. It saves the export in a folder of its
// own and lists its paths as they are in it (--absolute-names, which
// only the listing takes: tar would list them with their leading / and
// .. parts taken off), each on one line (GNU tar's escape quoting). It
// imports nothing when one is outside the agents' History, absolute or has a
// .. part, printing historyUnsafe instead. Every tar here is GNU tar,
// which Debian and Ubuntu have and base's Alpine branch installs over
// busybox's (TestHistoryImportHasGNUTarOnAlpine). GNU tar unpacks it
// in a folder of its own, without its owners or modes (the user's umask
// applies) and without importSkips (anchored, each with what's below
// it); the symlinks there that point outside it (absolute, or with a ..
// part) are taken out, so an agent's write never follows one out of its
// history. opencode's databases' WAL and journal files, and any of its
// databases that isn't a regular file, are taken out, and each database
// becomes its copy without its logins (opencodeClean), or is taken out
// when there's none, so an export made without it, which holds them,
// never gives them to a VM; then what's left of their History goes into the home folder,
// without replacing a file or folder the VM has (--skip-old-files,
// which implies --no-overwrite-dir).
func historyImport() string {
	historyPaths := agents.History()
	allowed := make([]string, len(historyPaths))
	for i, p := range historyPaths {
		allowed[i] = regexp.QuoteMeta(p)
	}
	own := []string{"--no-same-owner", "--no-same-permissions"}
	unpack := append(slices.Clone(own), "--anchored")
	for _, p := range importSkips() {
		unpack = append(unpack, "--exclude="+p)
	}
	return strings.Join([]string{
		"set -o pipefail",
		`mkdir -p ~/.cache && t=$(mktemp -d ~/.cache/devvm-import.XXXXXX) || exit 1`,
		`trap 'rm -rf "$t"' EXIT`,
		`cat > "$t/export" && tar -tzf "$t/export" --absolute-names --quoting-style=escape > "$t/names" || exit 1`,
		"if grep -vqE " + shell.Quote("^("+strings.Join(allowed, "|")+")(/|$)") + ` "$t/names" || ` +
			"grep -qE " + shell.Quote(`(^|/)\.\.(/|$)`) + ` "$t/names"; then echo ` + historyUnsafe + "; exit 1; fi",
		`mkdir "$t/x" && tar -xzf "$t/export" -C "$t/x" ` + shell.QuoteJoin(unpack) + " || exit 1",
		`find "$t/x" -type l -print0 | while IFS= read -r -d '' l; do`,
		`  case $(readlink "$l") in /* | .. | ../* | */.. | */../*) echo "devvm: left out the link $l" >&2; rm -f "$l" ;; esac`,
		"done",
		opencodeClean(),
		`o="$t/x/"` + shell.Quote(opencodeData),
		`for f in "$o"/` + strings.Join(opencodeDBs[1:], ` "$o"/`) + `; do rm -rf "$f"; done`,
		`for f in "$o"/` + opencodeDBs[0] + `; do if [ -L "$f" ] || { [ -e "$f" ] && [ ! -f "$f" ]; }; then rm -rf "$f"; fi; done`,
		`cd "$t" || exit 1`,
		opencodeCleanAll(`"$o"`, `mv clean.db "$db"`, `rm -f "$db"`),
		`cd "$t/x" && set -- && for p in ` + shell.QuoteJoin(historyPaths) + `; do if [ -e "$p" ] || [ -L "$p" ]; then set -- "$@" "$p"; fi; done`,
		`[ "$#" -eq 0 ] || tar -cf - "$@" | tar --extract --file=- --directory ~ --skip-old-files ` + shell.QuoteJoin(own),
	}, "\n")
}

// agentDirs returns an agent's settings folders, relative to home, a home
// folder or the agent's store (agentStore): its Dirs, and for a
// WholeFolder agent (opencode) its folder's subfolders there as they are
// now.
func agentDirs(home, agent string) []string {
	d := agents.Of(agent)
	dirs := slices.Clone(d.Dirs)
	if !d.WholeFolder {
		return dirs
	}
	for _, e := range readDir(filepath.Join(home, d.Folder)) {
		if e.IsDir() && folderSetting(e.Name()) {
			dirs = append(dirs, d.Folder+"/"+e.Name())
		}
	}
	return dirs
}

// folderSetting reports whether a folder of a WholeFolder agent's folder,
// by its name, is among its settings: all but node_modules.
func folderSetting(name string) bool { return name != "node_modules" }

// fileSetting reports whether a file of a WholeFolder agent's folder, by
// its name, is among its settings: all but lock files and the like.
func fileSetting(name string) bool {
	return !strings.HasSuffix(name, ".lock") && name != "package-lock.json" && name != ".gitignore" && name != ".DS_Store"
}

// agentFiles returns an agent's settings files, relative to home, a home
// folder or the agent's store (agentStore): its Files, and for a
// WholeFolder agent (opencode) its folder's files there, but lock files.
func agentFiles(home, agent string) []string {
	d := agents.Of(agent)
	var files []string
	for _, f := range d.Files {
		files = append(files, f.Rel)
	}
	if !d.WholeFolder {
		return files
	}
	for _, e := range readDir(filepath.Join(home, d.Folder)) {
		if e.Type().IsRegular() && fileSetting(e.Name()) {
			files = append(files, d.Folder+"/"+e.Name())
		}
	}
	return files
}

// readDir returns a folder's entries, but those whose names have a
// control character: the VM's setup takes paths one per line.
func readDir(dir string) []os.DirEntry {
	es, _ := os.ReadDir(dir)
	return slices.DeleteFunc(es, func(e os.DirEntry) bool { return strings.IndexFunc(e.Name(), unicode.IsControl) >= 0 })
}

// AgentMounts returns the host folders a VM with these shared tools
// mounts: its agents' settings folders that exist in devvm's store. One
// with a comma in its path is left out on every backend, with a warning:
// Lima splits mount options on commas.
func (e Launch) AgentMounts(tools []string) []string {
	dirs, comma := agentMounts(e.Dirs, tools)
	for _, p := range comma {
		e.Warn(msg.AgentDirComma(p))
	}
	return dirs
}

// agentMounts returns the folders of devvm's store a VM with these shared
// tools mounts (AgentMounts), and those it leaves out for a comma.
func agentMounts(d paths.Dirs, tools []string) (dirs, comma []string) {
	for _, a := range image.AgentsOf(tools) {
		store := agentStore(d, a)
		for _, rel := range agentDirs(store, a) {
			p := filepath.Join(store, rel)
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				continue
			}
			if strings.Contains(p, ",") {
				comma = append(comma, p)
				continue
			}
			dirs = append(dirs, p)
		}
	}
	return dirs, comma
}

// updateAgentDirs says how an existing VM's agent folder mounts change to
// match the store's folders, which can appear or go after it's made, from
// in, its instance. Each start of a stopped VM mounts the store's
// (starter.start): a stopped VM's change as its launch starts it, a
// running one's at its next start (later), which bringUp offers to make
// now, with whatever else waits for it, in its one question
// (restartForNextStart).
func (e Launch) updateAgentDirs(st *state.VM, in backend.Instance) (later bool) {
	vm := st.Name()
	want := e.AgentMounts(tools.Names(st))
	added := missing(want, in.AgentDirs)
	removed := missing(in.AgentDirs, want)
	if len(added) == 0 && len(removed) == 0 {
		return false
	}
	if len(added) > 0 {
		e.Warn(msg.AgentDirsAdded(shownList(added), vm))
	}
	if len(removed) > 0 {
		e.Warn(msg.AgentDirsRemoved(shownList(removed), vm))
	}
	if in.Status == backend.StatusRunning {
		return true
	}
	e.Info(msg.MountsUpdated(vm))
	return false
}

func shownList(paths []string) string { return strings.Join(paths, ", ") }

// claudeSlugMax is how long a slug of Claude Code's is before it's cut.
const claudeSlugMax = 200

// claudeSlug is the name of the folder Claude Code keeps a project
// folder's sessions in, under ~/.claude/projects, from the folder's path,
// as Claude Code computes it: each UTF-16 unit of the path that isn't an
// ASCII letter or digit becomes a dash, and a name longer than 200 is cut
// there and ends with a dash and its path's hash (claudeHash) in base 36.
func claudeSlug(path string) string {
	units := utf16.Encode([]rune(path))
	b := make([]byte, len(units))
	for i, u := range units {
		switch {
		case u >= 'a' && u <= 'z', u >= 'A' && u <= 'Z', u >= '0' && u <= '9':
			b[i] = byte(u)
		default:
			b[i] = '-'
		}
	}
	if len(b) <= claudeSlugMax {
		return string(b)
	}
	h := int64(claudeHash(units))
	if h < 0 {
		h = -h
	}
	return string(b[:claudeSlugMax]) + "-" + strconv.FormatInt(h, 36)
}

// claudeHash is the hash Claude Code ends a long slug with: h*31 + u over
// the path's UTF-16 units, in 32 bits.
func claudeHash(units []uint16) int32 {
	var h int32
	for _, u := range units {
		h = h*31 + int32(u)
	}
	return h
}

// missing returns what of a isn't in b.
func missing(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
