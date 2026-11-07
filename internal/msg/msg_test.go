package msg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/devvmhq/devvm/locales"
)

// sample makes message id with a value for each field: the field's name,
// and count for a plural message.
func sample(t *testing.T, id string, count int) M {
	t.Helper()
	var args []string
	for _, f := range specs[id].fields {
		v := "v_" + f
		if f == "count" {
			v = strconv.Itoa(count)
		}
		args = append(args, f+"="+v)
	}
	m, err := FromArgs(id, args)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return m
}

func TestLocalesLoad(t *testing.T) {
	b, err := newBundle(locales.FS)
	if err != nil {
		t.Fatal(err)
	}
	if tags := b.LanguageTags(); len(tags) == 0 || tags[0].String() != "en" {
		t.Errorf("languages %v: want English first", tags)
	}
}

// Every message renders in every language, with every value used.
func TestEveryMessageRenders(t *testing.T) {
	b := loadBundle()
	for id, s := range specs {
		for _, count := range []int{1, 2} {
			m := sample(t, id, count)
			for _, tag := range b.LanguageTags() {
				text := m.textIn(b, tag.String())
				if text == "" || strings.Contains(text, "<no value>") {
					t.Errorf("%s in %s: %q", id, tag, text)
				}
				// The prompt owns its answers: Ask prints [y/N] after the
				// question and Confirm [Y/n], so a message never has them.
				if strings.Contains(text, "[y/N]") || strings.Contains(text, "[Y/n]") {
					t.Errorf("%s in %s: %q has the answers its prompt prints", id, tag, text)
				}
				for _, f := range s.fields {
					if f != "count" && !strings.Contains(text, "v_"+f) {
						t.Errorf("%s in %s: %q doesn't show %s", id, tag, text, f)
					}
				}
				// Its segments join to the same text, each field a value.
				vals, joined := values(m.segmentsIn(b, tag.String()))
				for _, f := range s.fields {
					if f != "count" && !slices.Contains(vals, "v_"+f) {
						t.Errorf("%s in %s: segments %q have no value %s", id, tag, vals, f)
					}
				}
				if joined != text {
					t.Errorf("%s in %s: segments join to %q, want %q", id, tag, joined, text)
				}
			}
			if !s.plural {
				break
			}
		}
	}
}

func TestValuesAsGiven(t *testing.T) {
	v := `x y $HOME "q" <b> {{.name}}`
	if got, want := MsgUnknown(v).Text(), "There's no message "+v+"."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNames(t *testing.T) {
	m := MsgMissingValue("dir", "no_vm")
	if got, want := m.Names(), "msg_missing_value key=dir name=no_vm"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := HomeNotAbsolute().Names(), "home_not_absolute"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Whatever DEVVM_MESSAGES says, a message's String and Error are its
	// text: names mode is the UI's.
	t.Setenv("DEVVM_MESSAGES", "names")
	if got, want := m.String(), m.Text(); got != want {
		t.Errorf("String: got %q, want %q", got, want)
	}
	if got, want := m.Error(), m.Text(); got != want {
		t.Errorf("Error: got %q, want %q", got, want)
	}
}

// errors.Is and errors.As see through a message to its cause, which
// changes nothing it shows.
func TestBecause(t *testing.T) {
	t.Parallel()
	notExist := &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}
	plain := ProxyFailed("why")
	for _, tt := range []struct {
		name    string
		err     error
		is      error
		isWant  bool
		asPath  bool
		asNames string // the message errors.As finds
	}{
		{"path error", plain.Because(notExist), fs.ErrNotExist, true, true, "proxy_failed reason=why"},
		{"canceled", plain.Because(context.Canceled), context.Canceled, true, false, "proxy_failed reason=why"},
		{"wrapped message", fmt.Errorf("x: %w", plain.Because(notExist)), fs.ErrNotExist, true, true, "proxy_failed reason=why"},
		{"message in a message", NoSecret("s").Because(plain.Because(context.Canceled)), context.Canceled, true, false, "no_secret secret=s"},
		{"no cause", plain, fs.ErrNotExist, false, false, "proxy_failed reason=why"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.is); got != tt.isWant {
				t.Errorf("errors.Is(%v) = %v, want %v", tt.is, got, tt.isWant)
			}
			var pe *fs.PathError
			if got := errors.As(tt.err, &pe); got != tt.asPath {
				t.Errorf("errors.As *fs.PathError = %v, want %v", got, tt.asPath)
			}
			m, ok := errors.AsType[M](tt.err)
			if !ok || m.Names() != tt.asNames {
				t.Errorf("errors.As M = %q, %v, want %q", m.Names(), ok, tt.asNames)
			}
		})
	}
	if m := plain.Because(notExist); m.Error() != plain.Error() || m.Names() != plain.Names() {
		t.Errorf("with a cause: %q, want %q", m.Error(), plain.Error())
	}
}

func TestStringAddsDocLink(t *testing.T) {
	t.Parallel()
	m := MsgUnknown("x")
	if got := m.String(); got != m.Text() {
		t.Errorf("no doc: got %q, want the text", got)
	}
	m = BackendLacks("fake", "ssh", "devvm-x")
	if m.DocURL() == "" {
		t.Fatal("backend_lacks has no doc")
	}
	want := m.Text() + "\n" + m.DocURL()
	if got := m.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if text, link := m.Parts(); text != m.Text() || link != m.DocURL() {
		t.Errorf("Parts: got %q, %q, want the text and the link apart", text, link)
	}
}

func TestDocURL(t *testing.T) {
	for doc, want := range map[string]string{
		"":                                 "",
		"backends":                         "https://devvm.dev/docs/backends/",
		"using/settings#where-state-lives": "https://devvm.dev/docs/using/settings/#where-state-lives",
		"cli/devvm_ls":                     "https://devvm.dev/docs/cli/devvm_ls/",
	} {
		if got := docURL(doc); got != want {
			t.Errorf("docURL(%q) = %q, want %q", doc, got, want)
		}
	}
}

func TestUserLanguage(t *testing.T) {
	for _, tt := range []struct {
		env  map[string]string
		want string
	}{
		{nil, "en"},
		{map[string]string{"LANG": "de_DE.UTF-8"}, "de-DE"},
		{map[string]string{"LANG": "de_DE.UTF-8", "LC_MESSAGES": "fr_FR"}, "fr-FR"},
		{map[string]string{"LANG": "de_DE", "LC_MESSAGES": "fr_FR", "LC_ALL": "pt_BR.UTF-8"}, "pt-BR"},
		{map[string]string{"LANG": "sr_RS@latin"}, "sr-RS"},
		{map[string]string{"LANG": "de_DE", "LC_ALL": "C"}, "en"},
		{map[string]string{"LC_ALL": "POSIX"}, "en"},
		{map[string]string{"LC_ALL": "", "LANG": "ja_JP.UTF-8"}, "ja-JP"},
	} {
		if got := userLanguage(func(k string) string { return tt.env[k] }); got != tt.want {
			t.Errorf("%v: got %q, want %q", tt.env, got, tt.want)
		}
	}
}

// A translation is used for the messages it has; English for the rest.
// Plural forms follow the count.
func TestTranslationsAndPlurals(t *testing.T) {
	b, err := newBundle(fstest.MapFS{
		"en.toml": {Data: []byte(`
[t_vms]
one = "{{.count}} VM in {{.dir}}"
other = "{{.count}} VMs in {{.dir}}"
[t_plain]
other = "plain"
`)},
		"de.toml": {Data: []byte(`
[t_vms]
one = "{{.count}} VM in {{.dir}} (de)"
other = "{{.count}} VMs in {{.dir}} (de)"
`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	vms := func(n int) M {
		return M{id: "t_vms", plural: true, count: n, vals: []val{{"count", strconv.Itoa(n)}, {"dir", "/x"}}}
	}
	for _, tt := range []struct {
		m          M
		lang, want string
	}{
		{vms(1), "en", "1 VM in /x"},
		{vms(2), "en", "2 VMs in /x"},
		{vms(1), "de-DE", "1 VM in /x (de)"},
		{vms(2), "de", "2 VMs in /x (de)"},
		{vms(2), "fr", "2 VMs in /x"},
		{M{id: "t_plain"}, "de", "plain"},
		{M{id: "t_missing"}, "en", "t_missing"},
	} {
		if got := tt.m.textIn(b, tt.lang); got != tt.want {
			t.Errorf("%s in %s: got %q, want %q", tt.m.Names(), tt.lang, got, tt.want)
		}
	}
}

func TestBundleErrors(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"bad TOML":      {"en.toml": {Data: []byte("[x\n")}},
		"bad file name": {"not a tag.toml": {Data: []byte("")}},
		"unknown key":   {"en.toml": {Data: []byte("[x]\nother = \"x\"\ndoc = \"images\"\n")}},
	} {
		if _, err := newBundle(fsys); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestFromArgs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		id   string
		args []string
		want string // names of the message, or of the error
	}{
		{"msg_unknown", []string{"name=a b=c"}, "msg_unknown name=a b=c"},
		{"home_not_absolute", nil, "home_not_absolute"},
		{"msg_missing_value", []string{"name=n", "key=k"}, "msg_missing_value key=k name=n"},
		{"gc_tools", []string{"size=1G", "count=02"}, "gc_tools count=2 size=1G"},
		{"nope", nil, "msg_unknown name=nope"},
		{"msg_unknown", nil, "msg_missing_value key=name name=msg_unknown"},
		{"msg_unknown", []string{"name=a", "vm=b"}, "msg_unexpected_value key=vm name=msg_unknown"},
		{"msg_unknown", []string{"name=a", "name=b"}, "msg_repeated_value key=name"},
		{"msg_unknown", []string{"a"}, "msg_not_pair arg=a"},
		{"msg_unknown", []string{"=a"}, "msg_not_pair arg==a"},
		{"gc_tools", []string{"size=1G", "count=two"}, "msg_bad_count value=two"},
	} {
		m, err := FromArgs(tt.id, tt.args)
		got := m.Names()
		if err != nil {
			e, ok := errors.AsType[M](err)
			if !ok {
				t.Fatalf("%s %q: %v is not a message", tt.id, tt.args, err)
			}
			got = e.Names()
		}
		if got != tt.want {
			t.Errorf("%s %q: got %q, want %q", tt.id, tt.args, got, tt.want)
		}
	}
}

// .git protection's messages on Linux, which only a Linux host's runs
// show: each renders whole, its values in place, and each warning and
// refusal links to its page.
func TestGitLockMessages(t *testing.T) {
	for _, m := range []M{
		GitUnprotected("vm-1"), GitUnprotectedHelper("vm-1"), GitUnprotectedFs("vm-1"), GitProtectAvailable("vm-1"),
		GitProtectOnNeedsHelper(), GitProtectOnHelperUnusable(), GitProtectOnNeedsFs(),
		GitLockFailed("/p/.git/config"), GitStillLocked(".git/hooks, .git/config"),
		GitUnprotectedQuiet("vm-1"), GitChanged(".git/config", "vm-1"),
		DoctorImmutableMissing(), DoctorImmutableUnusable("it doesn't hold CAP_LINUX_IMMUTABLE"), DoctorImmutableNoSource(),
	} {
		text := m.Text()
		if strings.Contains(text, "{{") || strings.Contains(text, "<no value>") || m.DocURL() == "" {
			t.Errorf("%s: %q, link %q", m.ID(), text, m.DocURL())
		}
	}
	if text := DoctorImmutableInstall("/h", "/s").Text(); !strings.Contains(text, "-m 0755 /s /h and then sudo setcap cap_linux_immutable+ep /h ") {
		t.Errorf("doctor_immutable_install: %q", text)
	}
	if text := DoctorImmutableOK().Text(); text == "" {
		t.Error("doctor_immutable_ok is empty")
	}
}
