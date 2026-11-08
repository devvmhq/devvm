package vm

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
)

// plantedLogins are the planted logins of opencodeSchema's database,
// none of which an export or an import may carry, plantedKey first.
func plantedLogins() []string {
	return []string{plantedKey, "rt-devvm-planted-refresh", "at-devvm-planted-console", "devvm-planted-share-secret", "devvm-planted-vault-key"}
}

const (
	// plantedKey is the fake API key of plantedCredential.
	plantedKey = "sk-ant-devvm-planted-key"
	// keptSession is the title of the session the database keeps.
	keptSession = "devvm-kept-session"
)

// opencodeSchema is opencode's database in small: its migration
// records, opencode 2.2's credential, account and control_account
// tables, an earlier opencode's session_share, a table of a later
// opencode's with an api_key column, a session, and a trigger that puts
// a credential back when one is deleted. TestOpencodeCleanSchema checks
// opencode's own schema.
const opencodeSchema = `PRAGMA journal_mode = WAL;
CREATE TABLE migration (id TEXT PRIMARY KEY, time_completed INTEGER NOT NULL);
INSERT INTO migration VALUES ('` + opencodeAuthImport + `', 1), ('20261030054400_job', 1);
CREATE TABLE credential (id text PRIMARY KEY, integration_id text, label text NOT NULL, value text NOT NULL,
  connector_id text, method_id text, active integer, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE account (id text PRIMARY KEY, email text NOT NULL, url text NOT NULL, access_token text NOT NULL,
  refresh_token text NOT NULL, token_expiry integer, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE control_account (email text NOT NULL, url text NOT NULL, access_token text NOT NULL,
  refresh_token text NOT NULL, active integer NOT NULL, PRIMARY KEY (email, url));
CREATE TABLE session_share (session_id text PRIMARY KEY, id text NOT NULL, secret text NOT NULL, url text NOT NULL);
CREATE TABLE vault (id text PRIMARY KEY, api_key text);
CREATE TABLE session_v2 (id text PRIMARY KEY, title text NOT NULL, tokens_input integer DEFAULT 0 NOT NULL);
CREATE TABLE kv (key text PRIMARY KEY, value text NOT NULL);
CREATE TRIGGER back AFTER DELETE ON credential BEGIN
  INSERT INTO credential VALUES (old.id || 'x', old.integration_id, old.label, old.value, NULL, NULL, 1, 1, 1);
END;
INSERT INTO session_v2 VALUES ('ses_1', '` + keptSession + `', 5);
INSERT INTO kv VALUES ('wellknown:sources', '[]');
INSERT INTO account VALUES ('acc_1', 'a@example.com', 'https://opencode.ai', 'at-old', 'rt-devvm-planted-refresh', 0, 1, 1);
INSERT INTO control_account VALUES ('a@example.com', 'https://opencode.ai', 'at-devvm-planted-console', 'rt', 1);
INSERT INTO session_share VALUES ('ses_1', 'shr_1', 'devvm-planted-share-secret', 'https://opncd.ai/s/1');
INSERT INTO vault VALUES ('v1', 'devvm-planted-vault-key');
`

// plantedCredential is the credential row that opencodeDB inserts last,
// while its connection stays open, so it's in the WAL file alone.
const plantedCredential = `INSERT INTO credential VALUES ('cred_1', 'anthropic', 'API key',
  '{"type":"key","key":"sk-ant-devvm-planted-key"}', NULL, NULL, 1, 1, 1);
`

func needSqlite(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
}

func needGNUTar(t *testing.T) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), "tar", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("no GNU tar")
	}
}

// sqlite runs sql on the database at db with the sqlite3 CLI and
// returns what it printed.
func sqlite(t *testing.T, db, sql string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sqlite3", "-bail", db) //nolint:gosec // the test's own database
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite3 %s: %v: %s", db, err, out)
	}
	return string(out)
}

// opencodeDB makes opencode's database at db, as opencode keeps it while
// it runs: opencodeSchema checkpointed into the file, then
// plantedCredential in the WAL file of a connection that stays open
// until the test ends, as opencode's does.
func opencodeDB(t *testing.T, db string) {
	t.Helper()
	sqlite(t, db, opencodeSchema)
	cmd := exec.CommandContext(t.Context(), "sqlite3", "-bail", db) //nolint:gosec // the test's own database
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Wait()
	})
	if _, err := io.WriteString(in, "PRAGMA wal_autocheckpoint = 0;\n"+plantedCredential+"SELECT 'ready';\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(out)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("sqlite3 said %q, %v", line, err)
		}
		if line == "ready\n" {
			break
		}
	}
	if wal, err := os.ReadFile(db + "-wal"); err != nil || !bytes.Contains(wal, []byte(plantedKey)) { //nolint:gosec // the test's own file
		t.Fatalf("the WAL file doesn't hold the planted credential: %v", err)
	}
}

// checkCleanDB fails t when the database file at db holds any of the
// planted logins anywhere in its bytes, or a trigger, a WAL or journal
// file or opencodeAuthImport's record, or lacks the session.
func checkCleanDB(t *testing.T, db string) {
	t.Helper()
	data, err := os.ReadFile(db) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plantedLogins() {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("%s holds %s", db, s)
		}
	}
	if !bytes.Contains(data, []byte(keptSession)) {
		t.Errorf("%s lost the session", db)
	}
	// The file's header: a rollback journal's database, not WAL's.
	if len(data) < 20 || data[18] != 1 || data[19] != 1 {
		t.Errorf("%s is still in WAL mode", db)
	}
	for _, f := range []string{db + "-wal", db + "-journal"} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s is there", f)
		}
	}
	got := sqlite(t, db, "SELECT count(*) FROM sqlite_master WHERE type = 'trigger';\n"+
		"SELECT group_concat(id) FROM migration;\nSELECT count(*) FROM kv;\n")
	if got != "0\n20261030054400_job\n1\n" {
		t.Errorf("triggers, migrations and kv rows: %q", got)
	}
}

// runOpencodeClean runs devvm_opencode_clean on db, in db's folder, so
// the copy is clean.db beside it, and fails t when it prints anything.
func runOpencodeClean(t *testing.T, db string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", opencodeClean()+"\n"+`devvm_opencode_clean "$1"`, "bash", db) //nolint:gosec // devvm's own script
	cmd.Dir = filepath.Dir(db)
	// Its output is an export's tar stream: it prints nothing there.
	out, err := cmd.Output()
	if len(out) != 0 {
		t.Errorf("it printed %q", out)
	}
	return err
}

// opencodeClean's copy of a running opencode's database has its session
// and none of its logins, those its WAL file holds included, in no page
// of the file; a database it can't read gives no copy.
func TestOpencodeClean(t *testing.T) {
	needSqlite(t)
	t.Run("logins", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "opencode.db")
		opencodeDB(t, db)
		if err := runOpencodeClean(t, db); err != nil {
			t.Fatal(err)
		}
		checkCleanDB(t, filepath.Join(dir, "clean.db"))
	})
	t.Run("not a database", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "opencode.db")
		if err := state.WriteFile(db, []byte("sk-ant-devvm-planted-key, and no database")); err != nil {
			t.Fatal(err)
		}
		if err := runOpencodeClean(t, db); err == nil {
			t.Error("it made a copy")
		}
	})
}

// opencodeClean passes no sqlite3 -cmd, which sqlite3 3.54 with -bail
// runs alone, skipping the rest of its input and exiting 0, so the copy
// would be empty; and each of its sqlite3 runs turns trusted_schema off.
func TestOpencodeCleanNoCmd(t *testing.T) {
	script := opencodeClean()
	if strings.Contains(script, "-cmd") {
		t.Error("devvm_opencode_clean passes sqlite3 a -cmd")
	}
	runs, untrusted := strings.Count(script, "sqlite3 -bail"), strings.Count(script, "PRAGMA trusted_schema = OFF;")
	if runs != 6 || untrusted != runs {
		t.Errorf("%d sqlite3 runs, %d turn trusted_schema off", runs, untrusted)
	}
}

// opencodeClean fails closed with a sqlite3 that doesn't do what it's
// asked: one that runs nothing, prints nothing and exits 0, as sqlite3
// 3.54 did with -bail and an SQL -cmd, and one that copies the database
// but skips the DELETEs its stdin holds, so the logins stay.
func TestOpencodeCleanFailsClosed(t *testing.T) {
	needSqlite(t)
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"runs nothing": "#!/bin/sh\nexit 0\n",
		// With only -bail and the database, the SQL is on stdin.
		"skips deletes": "#!/bin/sh\nif [ $# -eq 2 ]; then grep -v '^DELETE' | " + shell.Quote(sqlite3) + ` "$@"; else exec ` +
			shell.Quote(sqlite3) + " \"$@\"; fi\n",
	}
	for name, stub := range stubs {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db := filepath.Join(dir, "opencode.db")
			opencodeDB(t, db)
			bin := t.TempDir()
			if err := state.WriteFileMode(filepath.Join(bin, "sqlite3"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := runOpencodeClean(t, db); err == nil {
				t.Error("it made a copy")
			}
			if name == "skips deletes" {
				// The stub did copy it, logins and all: the check caught it.
				if data, err := os.ReadFile(filepath.Join(dir, "clean.db")); err != nil || !bytes.Contains(data, []byte(plantedKey)) { //nolint:gosec // the test's own file
					t.Errorf("the stub's copy doesn't hold the planted key: %v", err)
				}
			}
		})
	}
}

// opencodeFixture is opencode 2.2.5's own schema, which sqlite3 printed
// in a VM; Maintaining says when to refresh it.
const opencodeFixture = "testdata/opencode-2.2.5.sql"

// The secrets of the kv rows plantSchemaRows adds: an organization's
// remote config, which opencode fetched with its well-known login.
const plantedWellknown = "devvm-planted-wellknown-config"

// plantSchemaRows fills db, made from opencodeFixture, with a row in
// every table: each text column of a table in emptied holds
// devvm-planted-TABLE-COLUMN, of any other table devvm-kept-TABLE-COLUMN,
// and kv holds well-known rows, one of them plantedWellknown's. It
// returns the kept values, and fails t on a table that is in neither
// emptied nor kept, so a refreshed fixture's new table gets looked at.
func plantSchemaRows(t *testing.T, db string, emptied, kept []string) []string {
	t.Helper()
	cols := map[string][][2]string{}
	for line := range strings.Lines(sqlite(t, db, "SELECT m.name, c.name, lower(c.type) FROM sqlite_master m "+
		"JOIN pragma_table_info(m.name) c WHERE m.type = 'table' ORDER BY m.name, c.cid;\n")) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "|")
		cols[f[0]] = append(cols[f[0]], [2]string{f[1], f[2]})
	}
	var sql strings.Builder
	var keptValues []string
	for table, cs := range cols {
		switch {
		case table == "kv" || table == "migration":
			continue
		case slices.Contains(emptied, table):
		case slices.Contains(kept, table):
		default:
			t.Errorf("%s's table %s is neither emptied nor kept: add it to one", opencodeFixture, table)
			continue
		}
		names, values := make([]string, 0, len(cs)), make([]string, 0, len(cs))
		for _, c := range cs {
			names = append(names, `"`+c[0]+`"`)
			if c[1] == "integer" || c[1] == "real" {
				values = append(values, "1")
				continue
			}
			v := "devvm-planted-" + table + "-" + c[0]
			if slices.Contains(kept, table) {
				v = "devvm-kept-" + table + "-" + c[0]
				keptValues = append(keptValues, v)
			}
			values = append(values, "'"+v+"'")
		}
		sql.WriteString(`INSERT INTO "` + table + `" (` + strings.Join(names, ", ") + ") VALUES (" + strings.Join(values, ", ") + ");\n")
	}
	sql.WriteString("INSERT INTO kv VALUES ('wellknown:sources', '[\"https://devvm-kept-org.example\"]', 1, 1), " +
		"('wellknown:manifest:https://devvm-kept-org.example', '{\"auth\":{\"command\":[\"devvm-kept-manifest\"]}}', 1, 1), " +
		"('wellknown:config:https://devvm-kept-org.example', '[{\"provider\":{\"org\":{\"options\":{\"apiKey\":\"" + plantedWellknown + "\"}}}}]', 1, 1);\n" +
		"INSERT INTO migration VALUES ('" + opencodeAuthImport + "', 1), ('20261030054400_job', 1);\n")
	sqlite(t, db, sql.String())
	return append(keptValues, "devvm-kept-org.example", "devvm-kept-manifest")
}

// opencodeClean's copy of a database made from opencode 2.2.5's own
// schema, a row planted in each table: no byte of it holds a value of a
// table that holds logins or the remote config a well-known login
// fetched, and every other table's row, the session's among them, is
// kept.
func TestOpencodeCleanSchema(t *testing.T) {
	needSqlite(t)
	schema, err := os.ReadFile(opencodeFixture)
	if err != nil {
		t.Fatal(err)
	}
	emptied := []string{"account", "account_state", "control_account", "credential"}
	kept := []string{
		"blob", "blob_ref", "event", "event_sequence", "instruction_blob", "instruction_entry", "instruction_state",
		"job", "permission", "project", "project_directory", "schedule", "session_inbox", "session_message",
		"session_part", "session_v2", "workspace", "worktree",
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "opencode.db")
	sqlite(t, db, "PRAGMA journal_mode = WAL;\n"+string(schema))
	keptValues := plantSchemaRows(t, db, emptied, kept)
	if err := runOpencodeClean(t, db); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "clean.db")
	data, err := os.ReadFile(copied) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"devvm-planted-", plantedWellknown} {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("the copy holds %s", s)
		}
	}
	for _, v := range keptValues {
		if !bytes.Contains(data, []byte(v)) {
			t.Errorf("the copy lost %s", v)
		}
	}
	if got := sqlite(t, copied, "SELECT group_concat(id) FROM migration;\nSELECT group_concat(key, ' ') FROM (SELECT key FROM kv ORDER BY key);\n"); got !=
		"20261030054400_job\nwellknown:manifest:https://devvm-kept-org.example wellknown:sources\n" {
		t.Errorf("migrations and kv keys: %q", got)
	}
}

// exportNames reads a gzipped tar stream: each member's name and, for a
// regular file, its content.
func exportNames(t *testing.T, export []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(export))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = data
	}
}

// pathWithout is a folder of links to the commands the history scripts
// run, sqlite3 among them only when with is true, for PATH.
func pathWithout(t *testing.T, with bool) string {
	t.Helper()
	dir := t.TempDir()
	cmds := []string{"bash", "cat", "find", "grep", "gzip", "ls", "mkdir", "mktemp", "mv", "readlink", "rm", "tar"}
	if with {
		cmds = append(cmds, "sqlite3")
	}
	for _, c := range cmds {
		p, err := exec.LookPath(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(dir, c)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The export script, run by GNU tar and sqlite3 as the VM's are (skipped
// where there's none): opencode's database, its WAL file holding a
// credential opencode just saved, goes as its copy without its logins,
// never with its WAL file, and the agents' logins stay out. Without
// sqlite3, the database stays out.
func TestHistoryExportRuns(t *testing.T) {
	needGNUTar(t)
	for _, with := range []bool{true, false} {
		t.Run(map[bool]string{true: "sqlite3", false: "no sqlite3"}[with], func(t *testing.T) {
			if with {
				needSqlite(t)
			}
			home := t.TempDir()
			mkdirs(t, home, ".claude/projects", opencodeData+"/storage", ".local/state/opencode", ".cache", piSessions+"/--p--")
			for _, f := range []string{
				".claude/projects/p.jsonl", ".claude/.credentials.json", opencodeData + "/auth.json",
				opencodeData + "/mcp-auth.json", opencodeData + "/storage/s.json", ".local/state/opencode/kv.json",
				piSessions + "/--p--/s.jsonl", piDir + "/auth.json", piDir + "/settings.json",
			} {
				if err := state.WriteFile(filepath.Join(home, f), []byte(f)); err != nil {
					t.Fatal(err)
				}
			}
			db := filepath.Join(home, opencodeData, "opencode.db")
			if with {
				opencodeDB(t, db)
			} else if err := state.WriteFile(db, []byte(plantedKey)); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", historyExport()) //nolint:gosec // devvm's own export script
			cmd.Env = []string{"HOME=" + home, "PATH=" + pathWithout(t, with)}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%v: %s", err, stderr.String())
			}
			got := exportNames(t, out)
			for f, want := range map[string]bool{
				".claude/projects/p.jsonl": true, opencodeData + "/storage/s.json": true, ".local/state/opencode/kv.json": true,
				".claude/.credentials.json": false, opencodeData + "/auth.json": false, opencodeData + "/mcp-auth.json": false,
				opencodeData + "/opencode.db": with, opencodeData + "/opencode.db-wal": false, opencodeData + "/opencode.db-shm": false,
				piSessions + "/--p--/s.jsonl": true, piDir + "/auth.json": false, piDir + "/settings.json": false,
			} {
				if _, ok := got[f]; ok != want {
					t.Errorf("%s: in the export %v, want %v", f, ok, want)
				}
			}
			for name := range got {
				if strings.Contains(name, "devvm-clean") || strings.HasPrefix(name, ".cache") {
					t.Errorf("the export has %s", name)
				}
			}
			if with {
				copied := filepath.Join(t.TempDir(), "opencode.db")
				if err := state.WriteFile(copied, got[opencodeData+"/opencode.db"]); err != nil {
					t.Fatal(err)
				}
				checkCleanDB(t, copied)
			} else if !strings.Contains(stderr.String(), "the VM has no sqlite3") {
				t.Errorf("it didn't say why: %q", stderr.String())
			}
			if es, _ := os.ReadDir(filepath.Join(home, ".cache")); len(es) != 0 {
				t.Errorf("its temp folder stayed: %v", es)
			}
		})
	}
}

// tarGz is a gzipped tarball of these files, in this order.
func tarGz(t *testing.T, names []string, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		h := &tar.Header{Name: n, Mode: 0o644, ModTime: time.Unix(1e9, 0), Typeflag: tar.TypeReg, Size: int64(len(files[n]))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// An export that holds opencode's database as it is, with its logins
// and a WAL file, as one made before devvm copied it without them does:
// the import puts in its copy without them, or with no sqlite3 in the
// VM, nothing of it.
func TestHistoryImportCleansOpencode(t *testing.T) {
	needGNUTar(t)
	for _, with := range []bool{true, false} {
		t.Run(map[bool]string{true: "sqlite3", false: "no sqlite3"}[with], func(t *testing.T) {
			if with {
				needSqlite(t)
			}
			db, wal := opencodeData+"/opencode.db", opencodeData+"/opencode.db-wal"
			files := map[string][]byte{
				db: []byte(plantedKey), wal: []byte(plantedKey),
				opencodeData + "/storage/s.json": []byte("s"),
			}
			if with {
				made := filepath.Join(t.TempDir(), "opencode.db")
				sqlite(t, made, opencodeSchema+plantedCredential+"PRAGMA journal_mode = DELETE;\n")
				data, err := os.ReadFile(made) //nolint:gosec // the test's own file
				if err != nil {
					t.Fatal(err)
				}
				files[db] = data
			}
			home := t.TempDir()
			cmd := exec.CommandContext(t.Context(), "bash", "-c", historyImport()) //nolint:gosec // devvm's own import script
			cmd.Env = []string{"HOME=" + home, "PATH=" + pathWithout(t, with)}
			cmd.Stdin = bytes.NewReader(tarGz(t, []string{db, wal, opencodeData + "/storage/s.json"}, files))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(home, opencodeData, "storage/s.json")); err != nil {
				t.Error(err)
			}
			if _, err := os.Lstat(filepath.Join(home, wal)); err == nil {
				t.Error("the WAL file was imported")
			}
			if !with {
				if _, err := os.Lstat(filepath.Join(home, db)); err == nil {
					t.Error("the database was imported without sqlite3")
				}
				return
			}
			checkCleanDB(t, filepath.Join(home, db))
		})
	}
}
