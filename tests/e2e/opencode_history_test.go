//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The fake API key planted in opencode's database in the VM, which no
// export may carry. It isn't an e2efake secret, which the section's
// later checks find in any file of the VM, as its database's free pages
// keep it once its row is deleted.
const opencodePlanted = "sk-ant-devvm-planted-opencode-key"

// The fake key planted in an organization's remote config, as opencode
// keeps one it fetched with a well-known login, in its kv table.
const opencodeWellknownPlanted = "sk-devvm-planted-wellknown-config"

// The fake private key of opencode's home, planted where opencode 2.4
// keeps it, in its folder opencodeMachine, which no export may carry.
const (
	opencodeMachine        = "~/.local/state/opencode/machine"
	opencodeHomeKeyPlanted = "devvm-planted-opencode-home-key"
)

// netOpencodeHistory: devvm history export of a VM whose opencode
// database holds its login's placeholders, a key typed into opencode
// and an organization's remote config (planted with the VM's sqlite3,
// as opencode's credential and kv rows) gives opencode's database as its
// copy without any, never its WAL file, and leaves the VM's own database
// as it was; and it leaves out opencode's home key, planted as a file.
func netOpencodeHistory(n *netRun) {
	s := n.s
	db := "~/.local/share/opencode/opencode.db"
	wellknown := "wellknown:config:https://e2e-org.example"
	s.Check("devvm history export leaves opencode's logins out of its database, the key typed into opencode in the VM too, and its home key out", func() error {
		plant := "sqlite3 " + db + ` "INSERT OR REPLACE INTO credential (id, integration_id, label, value, active, time_created, time_updated) ` +
			`VALUES ('cred_e2e_planted', 'e2e-provider', 'API key', '{\"type\":\"key\",\"key\":\"` + opencodePlanted + `\"}', 1, 1, 1); ` +
			`INSERT OR REPLACE INTO kv (key, value, time_created, time_updated) ` +
			`VALUES ('` + wellknown + `', '[{\"provider\":{\"org\":{\"options\":{\"apiKey\":\"` + opencodeWellknownPlanted + `\"}}}}]', 1, 1)"`
		if err := inVM(s, n.vm, plant); err != nil {
			return failed("planting a credential with the VM's sqlite3", err)
		}
		if err := inVM(s, n.vm, "mkdir -p "+opencodeMachine+" && printf '%s' "+opencodeHomeKeyPlanted+" > "+opencodeMachine+"/home-key"); err != nil {
			return failed("planting opencode's home key", err)
		}
		file := filepath.Join(s.sb.work, "opencode-export.tar.gz")
		_ = os.Remove(file)
		// The fake endpoints' company proxy, as the logins before it had:
		// the export's start writes the VM's proxy's upstream from it.
		if _, err := n.dv("", n.tokenEnv(), "history", "export", file); err != nil {
			return failed("devvm history export", err)
		}
		files, err := exportFiles(file)
		if err != nil {
			return err
		}
		got, ok := files[".local/share/opencode/opencode.db"]
		switch {
		case !ok:
			return errors.New("the export has no opencode.db")
		case bytes.Contains(got, []byte(opencodePlanted)), bytes.Contains(got, []byte(opencodeWellknownPlanted)),
			bytes.Contains(got, []byte("devvm-placeholder")):
			return errors.New("the export's opencode.db holds a login")
		}
		for name, data := range files {
			if strings.HasSuffix(name, ".db-wal") || strings.HasSuffix(name, ".db-shm") || strings.Contains(name, "devvm-clean") ||
				strings.HasPrefix(name, ".local/state/opencode/machine") || bytes.Contains(data, []byte(opencodeHomeKeyPlanted)) {
				return errors.New("the export has " + name)
			}
		}
		if has, err := vmHas(s, n.vm, true, "sqlite3 "+db+" 'SELECT value FROM credential'", opencodePlanted); err != nil || !has {
			return failed("the VM's own database keeps its credential", err)
		}
		return inVM(s, n.vm, "rm -rf "+opencodeMachine+" && sqlite3 "+db+
			` "DELETE FROM credential WHERE id = 'cred_e2e_planted'; DELETE FROM kv WHERE key = '`+wellknown+`'"`)
	})
}

// exportFiles is each regular file of the gzipped tar at path, by name.
func exportFiles(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = data
	}
}
