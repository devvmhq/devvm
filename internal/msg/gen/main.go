// Command gen writes internal/msg/messages_gen.go from locales/en.toml: one
// Go function per message, taking the template's fields as parameters. Run
// it with go generate ./internal/msg.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/token"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/BurntSushi/toml"
)

// in and out are relative to internal/msg, where go generate runs.
const (
	in  = "../../locales/en.toml"
	out = "messages_gen.go"
)

func main() {
	src, err := os.ReadFile(in)
	if err != nil {
		log.Fatal(err)
	}
	code, err := generate(src)
	if err != nil {
		log.Fatalf("%s: %v", in, err)
	}
	if err := os.WriteFile(out, code, 0o600); err != nil { //nolint:gosec // a fixed path
		log.Fatal(err)
	}
}

// table is one message in go-i18n's TOML format: the fields of
// i18n.Message, which go-i18n's loader and tools read.
type table struct {
	Description string `toml:"description"`
	Hash        string `toml:"hash"`
	LeftDelim   string `toml:"leftDelim"`
	RightDelim  string `toml:"rightDelim"`
	Zero        string `toml:"zero"`
	One         string `toml:"one"`
	Two         string `toml:"two"`
	Few         string `toml:"few"`
	Many        string `toml:"many"`
	Other       string `toml:"other"`
}

// message is one table of the message file.
type message struct {
	id     string
	other  string
	one    string
	fields []string // the template's fields, sorted; "count" when plural
}

func (m message) plural() bool { return m.one != "" }

// idRe is a message ID or a template field: snake_case.
var idRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// parseMessages reads and checks the message file. Each table is a
// message; a key go-i18n doesn't know, or a nested table, is refused.
func parseMessages(src []byte) ([]message, error) {
	var raw map[string]table
	md, err := toml.Decode(string(src), &raw)
	if err != nil {
		return nil, err
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		k := keys[0]
		return nil, fmt.Errorf("[%s]: unknown key %q (go-i18n's message fields only)", k[0], strings.Join(k[1:], "."))
	}
	var msgs []message
	for id, t := range raw {
		m, err := parseMessage(id, t)
		if err != nil {
			return nil, fmt.Errorf("[%s]: %w", id, err)
		}
		msgs = append(msgs, m)
	}
	slices.SortFunc(msgs, func(a, b message) int { return strings.Compare(a.id, b.id) })
	return msgs, nil
}

func parseMessage(id string, t table) (message, error) {
	if !idRe.MatchString(id) {
		return message{}, errors.New("the ID must be snake_case")
	}
	if t.Zero != "" || t.Two != "" || t.Few != "" || t.Many != "" {
		return message{}, errors.New("plurals in English are one and other only")
	}
	if t.LeftDelim != "" || t.RightDelim != "" {
		return message{}, errors.New("templates use {{ }}: no leftDelim or rightDelim")
	}
	m := message{id: id, other: t.Other, one: t.One}
	if m.other == "" {
		return message{}, errors.New("no other text")
	}
	fields := map[string]bool{}
	for _, text := range []string{m.other, m.one} {
		if err := templateFields(text, fields); err != nil {
			return message{}, err
		}
	}
	if m.plural() {
		fields["count"] = true
	} else if fields["count"] {
		return message{}, errors.New("{{.count}} is for plural messages: add a one text")
	}
	for f := range fields {
		if !idRe.MatchString(f) {
			return message{}, fmt.Errorf("field %q must be snake_case", f)
		}
		if p := goName(f, false); token.IsKeyword(p) || taken[p] {
			return message{}, fmt.Errorf("field %q is taken in Go; rename it", f)
		}
		m.fields = append(m.fields, f)
	}
	slices.Sort(m.fields)
	return m, nil
}

// templateFields adds the fields text uses ({{.dir}}) to fields. Only
// plain fields are allowed: messages hold no logic.
func templateFields(text string, fields map[string]bool) error {
	if text == "" {
		return nil
	}
	tmpl, err := template.New("").Parse(text)
	if err != nil {
		return err
	}
	for _, n := range tmpl.Root.Nodes {
		switch n := n.(type) {
		case *parse.TextNode:
		case *parse.ActionNode:
			f, ok := plainField(n)
			if !ok {
				return fmt.Errorf("%s: only plain fields like {{.dir}} are allowed", n)
			}
			fields[f] = true
		default:
			return fmt.Errorf("%s: only plain fields like {{.dir}} are allowed", n)
		}
	}
	return nil
}

func plainField(n *parse.ActionNode) (string, bool) {
	if len(n.Pipe.Decl) > 0 || len(n.Pipe.Cmds) != 1 || len(n.Pipe.Cmds[0].Args) != 1 {
		return "", false
	}
	f, ok := n.Pipe.Cmds[0].Args[0].(*parse.FieldNode)
	if !ok || len(f.Ident) != 1 {
		return "", false
	}
	return f.Ident[0], true
}

// taken are names a parameter would shadow in the generated functions.
var taken = map[string]bool{
	"any": true, "bool": true, "byte": true, "error": true, "false": true, "int": true,
	"nil": true, "string": true, "true": true, "len": true, "new": true, "make": true,
	"strconv": true, "val": true, "spec": true,
}

// initialisms are written in capitals in Go names: vm_name is vmName,
// no_vm is NoVM.
var initialisms = map[string]bool{
	"api": true, "ca": true, "cli": true, "cpu": true, "dns": true, "gc": true, "http": true,
	"https": true, "id": true, "ip": true, "json": true, "kvm": true, "oci": true, "ok": true, "os": true, "pid": true,
	"qemu": true, "sha": true, "ssh": true, "tcp": true, "tls": true, "tty": true, "ui": true, "uid": true,
	"url": true, "vm": true, "vms": true, "xdg": true,
}

// goName turns snake_case into a Go name, exported or not.
func goName(s string, exported bool) string {
	var b strings.Builder
	for i, w := range strings.Split(s, "_") {
		switch {
		case i == 0 && !exported:
			b.WriteString(w)
		case w == "vms":
			b.WriteString("VMs")
		case initialisms[w]:
			b.WriteString(strings.ToUpper(w))
		default:
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return b.String()
}

// generate returns messages_gen.go for the message file src.
func generate(src []byte) ([]byte, error) {
	msgs, err := parseMessages(src)
	if err != nil {
		return nil, err
	}
	funcs := map[string]string{}
	for _, m := range msgs {
		name := goName(m.id, true)
		if other, ok := funcs[name]; ok {
			return nil, fmt.Errorf("[%s] and [%s] are both %s in Go", other, m.id, name)
		}
		funcs[name] = m.id
	}

	var b bytes.Buffer
	b.WriteString("// Code generated by go generate ./internal/msg from locales/en.toml. DO NOT EDIT.\n\n")
	b.WriteString("package msg\n\n")
	var imports []string
	if slices.ContainsFunc(msgs, message.plural) {
		imports = append(imports, `"strconv"`)
	}
	if slices.ContainsFunc(msgs, func(m message) bool { return slices.ContainsFunc(m.fields, vmField) }) {
		// The module's own after the standard library's, apart.
		imports = append(imports, "\n"+`"github.com/devvmhq/devvm/internal/vmname"`)
	}
	if len(imports) > 0 {
		fmt.Fprintf(&b, "import (\n%s\n)\n\n", strings.Join(imports, "\n"))
	}
	for _, m := range msgs {
		writeFunc(&b, m)
	}
	b.WriteString("// specs describes every message, by ID.\nvar specs = map[string]spec{\n")
	for _, m := range msgs {
		var s []string
		if m.plural() {
			s = append(s, "plural: true")
		}
		if len(m.fields) > 0 {
			s = append(s, fmt.Sprintf("fields: %#v", m.fields))
		}
		fmt.Fprintf(&b, "\t%q: {%s},\n", m.id, strings.Join(s, ", "))
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

// vmField reports whether a message's field f is a VM's name, which its
// function takes as a vmname.VM: vm, or vm_name.
func vmField(f string) bool { return f == "vm" || f == "vm_name" }

func writeFunc(b *bytes.Buffer, m message) {
	var params, vals []string
	for _, f := range m.fields {
		p := goName(f, false)
		if f == "count" {
			params = append(params, p+" int")
			vals = append(vals, fmt.Sprintf("{%q, strconv.Itoa(%s)}", f, p))
			continue
		}
		if vmField(f) {
			params = append(params, p+" vmname.VM")
			vals = append(vals, fmt.Sprintf("{%q, string(%s)}", f, p))
			continue
		}
		params = append(params, p+" string")
		vals = append(vals, fmt.Sprintf("{%q, %s}", f, p))
	}
	fmt.Fprintf(b, "// %s is [%s]: %q\n", goName(m.id, true), m.id, m.other)
	fmt.Fprintf(b, "func %s(%s) M {\n", goName(m.id, true), strings.Join(params, ", "))
	fmt.Fprintf(b, "\treturn M{id: %q", m.id)
	if m.plural() {
		b.WriteString(", plural: true, count: count")
	}
	if len(vals) > 0 {
		fmt.Fprintf(b, ", vals: []val{%s}", strings.Join(vals, ", "))
	}
	b.WriteString("}\n}\n\n")
}
