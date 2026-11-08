package vm

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/shell"
)

// What devvm agent save reads from a VM at most: the whole stream, one
// file, and its files, folders and links together. More fails the save,
// saving nothing.
const (
	saveMaxBytes   = 128 << 20
	saveMaxFile    = 16 << 20
	saveMaxEntries = 20000
	// saveLinkHops is how many links a link may lead through to its file.
	saveLinkHops = 8
	// saveOwn is the folder of the save's own files in the VM's stream:
	// home, the VM's home folder; claude.json, its ~/.claude.json's
	// subset; claude-unreadable, there when that isn't JSON.
	saveOwn = ".devvm-save"
)

// agentSaveScript is the script that sends a VM's settings of these
// agents as a tar stream on its stdout, run there as the VM's user: the
// files and folders their Files and Dirs name, a WholeFolder agent's
// (opencode's) folder's entries but node_modules, as they are (a link as
// a link), and the save's own files (saveOwn): its home folder and, for
// claude, what devvm's store keeps of ~/.claude.json
// (agentlogin.ClaudeTopKeys), so its per-project entries never leave the
// VM. The host checks all of it (readSent): the VM is trusted with
// nothing.
func agentSaveScript(names []string) string {
	var paths []string
	for _, a := range names {
		d := agents.Of(a)
		for _, f := range d.Files {
			paths = append(paths, f.Rel)
		}
		paths = append(paths, d.Dirs...)
	}
	lines := []string{
		"set -o pipefail",
		"cd ~ || exit 1",
		`t=$(mktemp -d) || exit 1`,
		`trap 'rm -rf "$t"' EXIT`,
		`mkdir "$t/` + saveOwn + `" && printf '%s\n' "$HOME" > "$t/` + saveOwn + `/home" || exit 1`,
		"set --",
	}
	if len(paths) > 0 {
		lines = append(lines, `for p in `+shell.QuoteJoin(paths)+`; do if [ -e "$p" ] || [ -L "$p" ]; then set -- "$@" "$p"; fi; done`)
	}
	for _, a := range names {
		if d := agents.Of(a); d.WholeFolder {
			lines = append(lines,
				`for p in `+d.Folder+`/* `+d.Folder+`/.[!.]* `+d.Folder+`/..?*; do`,
				`  if { [ -e "$p" ] || [ -L "$p" ]; } && [ "${p##*/}" != node_modules ]; then set -- "$@" "$p"; fi`,
				"done")
		}
	}
	if slices.Contains(names, agents.Claude) {
		keys, _ := json.Marshal(agentlogin.ClaudeTopKeys())
		own := `"$t/` + saveOwn
		lines = append(lines,
			`if [ -f `+agents.ClaudeJSON+` ] && [ ! -L `+agents.ClaudeJSON+` ]; then`,
			`  jq --argjson k `+shell.Quote(string(keys))+` 'with_entries(select(.key as $x | any($k[]; . == $x)))' `+agents.ClaudeJSON+` > `+own+`/claude.json" 2>/dev/null ||`,
			`    { rm -f `+own+`/claude.json"; : > `+own+`/claude-unreadable"; }`,
			"fi")
	}
	lines = append(lines, `tar -cf - "$@" -C "$t" `+saveOwn)
	return strings.Join(lines, "\n")
}

// saveMaxStderr is the most of the save script's stderr that goes into
// the run log.
const saveMaxStderr = 1 << 20

// sentEntry is one entry of the VM's stream in the agents' settings: a
// folder ('d'), a file ('f') with its data and whether its owner may run
// it, a link ('l') with its target, or another kind (0), which is left
// out; the agent it's of and the listed file or folder it is or is in
// (top, a folder when folder).
type sentEntry struct {
	kind   byte
	data   []byte
	exec   bool
	link   string
	agent  string
	top    string
	folder bool
}

// sent is what a VM sent of its agents' settings (readSent): its home
// folder, what devvm's store keeps of its ~/.claude.json (or that it
// isn't JSON), and its entries by their paths relative to its home.
type sent struct {
	vm        string
	home      string
	claude    []byte
	claudeBad bool
	entries   map[string]*sentEntry
}

// readSent reads the tar stream a VM sent (agentSaveScript) for these
// agents. Every path must be one of their settings (listedPath), or in
// one of their settings folders inside a folder the stream has, each
// once, relative, clean and of characters a terminal shows; a file at most
// saveMaxFile, and at most saveMaxEntries of them all. Any other is an
// error, and nothing is saved.
func readSent(r io.Reader, vm string, agents []string) (*sent, error) {
	s := &sent{vm: vm, entries: map[string]*sentEntry{}}
	tr := tar.NewReader(r)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return s, nil
		} else if err != nil {
			return nil, msg.AgentSaveNotRead(vm).Because(err)
		}
		if n >= saveMaxEntries {
			return nil, msg.AgentSaveTooMuch(vm)
		}
		name, ok := entryName(h.Name)
		if !ok {
			return nil, msg.AgentSaveUnsafe(h.Name, vm)
		}
		if err := s.add(tr, h, name, agents); err != nil {
			return nil, err
		}
	}
}

// entryName is a stream entry's path relative to the VM's home, a
// folder's without its last /, when it's one devvm takes: relative,
// clean (no . or .. part), valid UTF-8 and only of characters a terminal
// shows (unicode.IsGraphic: no control, format or bidi characters), so
// the diff shows each name as it is.
func entryName(raw string) (string, bool) {
	name := strings.TrimSuffix(raw, "/")
	ok := name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		name != ".." && !strings.HasPrefix(name, "../") && utf8.ValidString(name) &&
		strings.IndexFunc(name, func(r rune) bool { return !unicode.IsGraphic(r) }) < 0
	return name, ok
}

// add adds the entry h, at name, its data read from tr.
func (s *sent) add(tr io.Reader, h *tar.Header, name string, agents []string) error {
	if name == saveOwn || strings.HasPrefix(name, saveOwn+"/") {
		return s.own(tr, h, name)
	}
	unsafe := msg.AgentSaveUnsafe(name, s.vm)
	agent, top, folder, ok := listedPath(agents, name, h.Typeflag == tar.TypeDir)
	if _, dup := s.entries[name]; dup || !ok {
		return unsafe
	}
	if name != top {
		if p := s.entries[path.Dir(name)]; p == nil || p.kind != 'd' {
			return unsafe // in no folder the stream has, such as one below a link
		}
	}
	e := &sentEntry{agent: agent, top: top, folder: folder}
	switch h.Typeflag {
	case tar.TypeDir:
		e.kind = 'd'
	case tar.TypeReg:
		data, err := readCapped(tr, h, saveMaxFile)
		if err != nil {
			return s.tooMuch(err)
		}
		e.kind, e.data, e.exec = 'f', data, h.Mode&0o100 != 0
	case tar.TypeSymlink:
		e.kind, e.link = 'l', h.Linkname
	}
	s.entries[name] = e
	return nil
}

// errTooBig is readCapped's error for a file over its limit.
var errTooBig = errors.New("too big")

// readCapped reads the file h, at most limit bytes.
func readCapped(r io.Reader, h *tar.Header, limit int64) ([]byte, error) {
	if h.Size > limit {
		return nil, errTooBig
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errTooBig
	}
	return data, err
}

// tooMuch is the save's error for a file readCapped refused.
func (s *sent) tooMuch(err error) error {
	if errors.Is(err, errTooBig) {
		return msg.AgentSaveTooMuch(s.vm)
	}
	return msg.AgentSaveNotRead(s.vm).Because(err)
}

// own takes one of the save's own entries (saveOwn): only its folder and
// its files, each a file.
func (s *sent) own(tr io.Reader, h *tar.Header, name string) error {
	if name == saveOwn && h.Typeflag == tar.TypeDir {
		return nil
	}
	file, _ := strings.CutPrefix(name, saveOwn+"/")
	if h.Typeflag != tar.TypeReg || !slices.Contains([]string{"home", "claude.json", "claude-unreadable"}, file) {
		return msg.AgentSaveUnsafe(name, s.vm)
	}
	data, err := readCapped(tr, h, saveMaxFile)
	if err != nil {
		return s.tooMuch(err)
	}
	switch file {
	case "home":
		home := strings.TrimSuffix(string(data), "\n")
		if path.IsAbs(home) && path.Clean(home) == home && msg.OneLine(home) == home {
			s.home = home
		}
	case "claude.json":
		s.claude = data
	default:
		s.claudeBad = true
	}
	return nil
}

// listedPath returns the agent, of these, whose settings name is, or is
// in: the file or folder it's listed as (top), a folder when folder.
// Those are their Files and Dirs, and a WholeFolder agent's (opencode's)
// folder's entries but its lock files and node_modules (fileSetting,
// folderSetting), by whether the entry is a folder (dir).
func listedPath(names []string, name string, dir bool) (agent, top string, folder, ok bool) {
	for _, a := range names {
		d := agents.Of(a)
		if slices.ContainsFunc(d.Files, func(f agents.File) bool { return f.Rel == name }) {
			return a, name, false, true
		}
		for _, p := range d.Dirs {
			if name == p || strings.HasPrefix(name, p+"/") {
				return a, p, true, true
			}
		}
		rest, in := strings.CutPrefix(name, d.Folder+"/")
		if !d.WholeFolder || !in {
			continue
		}
		first, _, below := strings.Cut(rest, "/")
		switch {
		case (below || dir) && folderSetting(first):
			return a, d.Folder + "/" + first, true, true
		case !below && !dir && fileSetting(first):
			return a, name, false, true
		}
	}
	return "", "", false, false
}

// resolve returns the file a link in a settings folder, at name, leads
// to: by relative paths only, through at most saveLinkHops links, each
// step inside top, the folder, through folders the stream has. None for
// any other link, or one to a folder.
func (s *sent) resolve(name, top string) (*sentEntry, bool) {
	for range saveLinkHops {
		e := s.entries[name]
		switch {
		case e == nil:
			return nil, false
		case e.kind == 'f':
			return e, true
		case e.kind != 'l' || e.link == "" || path.IsAbs(e.link):
			return nil, false
		}
		next := path.Join(path.Dir(name), e.link)
		if !strings.HasPrefix(next, top+"/") {
			return nil, false
		}
		for d := path.Dir(next); d != top; d = path.Dir(d) {
			if p := s.entries[d]; p == nil || p.kind != 'd' {
				return nil, false
			}
		}
		name = next
	}
	return nil, false
}

// names is the stream's paths of the agent's settings, sorted, so a
// folder comes before what's in it.
func (s *sent) names(agent string) []string {
	var out []string
	for n, e := range s.entries {
		if e.agent == agent {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
