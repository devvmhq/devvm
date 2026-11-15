// Package msg holds every message devvm prints, as typed functions generated
// from locales/en.toml (NoVM(dir) for [no_vm]). It renders them in the
// user's language, but never prints: internal/ui does.
//
// A message's names are its ID and values in sorted key order (no_vm
// dir=/x): internal/ui shows them in place of its text with
// DEVVM_MESSAGES=names, so tests never depend on wording or language.
package msg

//go:generate go run ./gen

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"

	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/locales"
)

// DocsBase is the docs site's address; doc links are under it.
const DocsBase = "https://devvm.dev/docs/"

// M is a message with its values, made by the generated functions, and the
// error it reports, if any (Because).
type M struct {
	id     string
	plural bool
	count  int
	vals   []val // in sorted key order
	cause  error
}

type val struct{ key, value string }

// spec is what the generated table says about a message.
type spec struct {
	plural bool
	fields []string // sorted; "count" when plural
}

// ID returns the message's ID, its table name in en.toml.
func (m M) ID() string { return m.id }

// String returns the message as devvm prints it: its text, then its doc
// link on a line of its own.
func (m M) String() string {
	text, link := m.Parts()
	if link != "" {
		return text + "\n" + link
	}
	return text
}

// Parts returns what String joins, apart, so a terminal can color the
// link: the message's text and its doc link ("" when it has none).
func (m M) Parts() (text, link string) {
	return m.Text(), m.DocURL()
}

// Error makes a message usable as an error. It's the same as String.
func (m M) Error() string { return m.String() }

// Because returns the message carrying err, the error it reports, so
// errors.Is and errors.As see through it. The text stays the message's own.
func (m M) Because(err error) M {
	m.cause = err
	return m
}

// Unwrap returns the error the message reports, or nil.
func (m M) Unwrap() error { return m.cause }

// Value returns the value of the message's field key, or "" when it has
// none, so a caller can name what another message names.
func (m M) Value(key string) string {
	for _, v := range m.vals {
		if v.key == key {
			return v.value
		}
	}
	return ""
}

// VM returns the VM the message's vm field names, as its function took
// it, or "" when it has none.
func (m M) VM() vmname.VM { return vmname.VM(m.Value("vm")) }

// Names returns the message's ID and values: no_vm dir=/x.
func (m M) Names() string {
	var b strings.Builder
	b.WriteString(m.id)
	for _, v := range m.vals {
		b.WriteString(" " + v.key + "=" + OneLine(v.value))
	}
	return b.String()
}

// Text returns the message's text in the user's language, falling back to
// English when it has no translation.
func (m M) Text() string {
	return m.textIn(loadBundle(), userLanguage(os.Getenv))
}

func (m M) textIn(b *i18n.Bundle, lang string) string { return m.renderIn(b, lang, OneLine) }

// renderIn is the message's text with each value as value makes it, which
// goes through OneLine: values are made by anyone, a VM included, so each
// goes on one line without what Safe refuses, wherever the message is shown.
func (m M) renderIn(b *i18n.Bundle, lang string, value func(string) string) string {
	data := make(map[string]string, len(m.vals))
	for _, v := range m.vals {
		data[v.key] = value(v.value)
	}
	lc := &i18n.LocalizeConfig{MessageID: m.id, TemplateData: data}
	if m.plural {
		lc.PluralCount = m.count
	}
	text, err := i18n.NewLocalizer(b, lang).Localize(lc)
	if text == "" && err != nil {
		// Only a message missing from en.toml gets here, which the
		// generated file rules out.
		return m.Names()
	}
	return text
}

// DocURL returns the address of the message's doc page (from docs), or ""
// when it has none.
func (m M) DocURL() string { return docURL(docs[m.id]) }

// docURL turns "page#anchor" into the page's address on the docs site.
func docURL(doc string) string {
	if doc == "" {
		return ""
	}
	page, anchor, _ := strings.Cut(doc, "#")
	u := DocsBase + page + "/"
	if anchor != "" {
		u += "#" + anchor
	}
	return u
}

// userLanguage returns the language messages are shown in, from the first
// of LC_ALL, LC_MESSAGES and LANG that is set, as a BCP 47 tag ("de-DE"
// for de_DE.UTF-8). C and POSIX are English.
func userLanguage(getenv func(string) string) string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := getenv(name)
		if v == "" {
			continue
		}
		v, _, _ = strings.Cut(v, ".")
		v, _, _ = strings.Cut(v, "@")
		if v == "C" || v == "POSIX" {
			return "en"
		}
		return strings.ReplaceAll(v, "_", "-")
	}
	return "en"
}

// loadBundle returns every embedded locale, English first.
var loadBundle = sync.OnceValue(func() *i18n.Bundle {
	b, err := newBundle(locales.FS)
	if err != nil {
		// The files are embedded and checked by the tests.
		panic(err)
	}
	return b
})

// newBundle loads every *.toml file in fsys, named by language (en.toml),
// with go-i18n's own loader.
func newBundle(fsys fs.FS) (*i18n.Bundle, error) {
	b := i18n.NewBundle(language.English)
	b.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	files, err := fs.Glob(fsys, "*.toml")
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		// go-i18n files a name that isn't a language under "und" without
		// an error, so the name is checked here.
		if _, err := language.Parse(strings.TrimSuffix(path.Base(file), ".toml")); err != nil {
			return nil, &fs.PathError{Op: "load", Path: file, Err: err}
		}
		if _, err := b.LoadMessageFileFS(fsys, file); err != nil {
			return nil, &fs.PathError{Op: "load", Path: file, Err: err}
		}
	}
	return b, nil
}

// FromArgs makes a message from its ID and key=value arguments, checked
// against its fields: what devvm _msg prints. A plural message's count is
// given as count=N.
func FromArgs(id string, args []string) (M, error) {
	s, ok := specs[id]
	if !ok {
		return M{}, MsgUnknown(id)
	}
	given := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return M{}, MsgNotPair(a)
		}
		if _, dup := given[k]; dup {
			return M{}, MsgRepeatedValue(k)
		}
		given[k] = v
	}
	m := M{id: id, plural: s.plural}
	for _, f := range s.fields {
		v, ok := given[f]
		if !ok {
			return M{}, MsgMissingValue(f, id)
		}
		delete(given, f)
		if f == "count" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return M{}, MsgBadCount(v)
			}
			m.count, v = n, strconv.Itoa(n)
		}
		m.vals = append(m.vals, val{f, v})
	}
	if len(given) > 0 {
		return M{}, MsgUnexpectedValue(slices.Sorted(maps.Keys(given))[0], id)
	}
	return m, nil
}
