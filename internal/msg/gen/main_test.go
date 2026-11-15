package main

import (
	"os"
	"strings"
	"testing"
)

func TestGeneratedFileIsCurrent(t *testing.T) {
	src, err := os.ReadFile("../../../locales/en.toml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := generate(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../messages_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("internal/msg/messages_gen.go is stale: run go generate ./internal/msg")
	}
}

func TestGenerate(t *testing.T) {
	code, err := generate([]byte(`
[no_vm]
description = "the current folder has no VM"
other = "no VM for {{.dir}}: run devvm there to create one"

[vm_count]
one = "{{.vm_name}} has {{.count}} VM"
other = "{{.vm_name}} has {{.count}} VMs"

[ssh_key_id]
other = "key {{.key_id}} for {{.url}} on {{.vms}}"

[ready]
other = "ready"
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`func NoVM(dir string) M {`,
		`return M{id: "no_vm", vals: []val{{"dir", dir}}}`,
		`func VMCount(count int, vmName vmname.VM) M {`,
		`return M{id: "vm_count", plural: true, count: count, vals: []val{{"count", strconv.Itoa(count)}, {"vm_name", string(vmName)}}}`,
		`func SSHKeyID(keyID string, url string, vms string) M {`,
		`func Ready() M {`,
		`return M{id: "ready"}`,
		`"strconv"`,
		`"github.com/devvmhq/devvm/internal/vmname"`,
		`"no_vm":      {fields: []string{"dir"}},`,
		`"vm_count":   {plural: true, fields: []string{"count", "vm_name"}},`,
		`"ready":      {},`,
	} {
		if !strings.Contains(string(code), want) {
			t.Errorf("no %s in:\n%s", want, code)
		}
	}

	code, err = generate([]byte("[ready]\nother = \"ready\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(code), "strconv") || strings.Contains(string(code), "vmname") {
		t.Errorf("strconv or vmname imported without plurals or VMs:\n%s", code)
	}
}

func TestGenerateRefuses(t *testing.T) {
	for _, tt := range []struct{ name, src, want string }{
		{"bad TOML", `[x`, "expected"},
		{"camelCase ID", "[noVM]\nother = \"x\"", "snake_case"},
		{"no other", "[x]\none = \"x\"", "no other text"},
		{"unknown key", "[x]\nother = \"x\"\ndoc = \"images#where-state-lives\"", `[x]: unknown key "doc"`},
		{"only unknown keys", "[x]\ndoc = \"y\"", `[x]: unknown key "doc"`},
		{"nested table", "[x.y]\nother = \"x\"", `[x]: unknown key "y"`},
		{"not a table", "x = \"y\"", "expected table but found string"},
		{"not a string", "[x]\nother = 1", "incompatible types"},
		{"other plural forms", "[x]\nother = \"x\"\nfew = \"y\"", "one and other only"},
		{"delimiters", "[x]\nother = \"<<.a>>\"\nleftDelim = \"<<\"\nrightDelim = \">>\"", "no leftDelim or rightDelim"},
		{"camelCase field", "[x]\nother = \"{{.vmName}}\"", `field "vmName" must be snake_case`},
		{"nested field", "[x]\nother = \"{{.vm.name}}\"", "only plain fields"},
		{"logic", "[x]\nother = \"{{if .a}}a{{end}}\"", "only plain fields"},
		{"a function", "[x]\nother = \"{{printf .a}}\"", "only plain fields"},
		{"a variable", "[x]\nother = \"{{$a := .a}}\"", "only plain fields"},
		{"bad template", "[x]\nother = \"{{.a\"", "unclosed action"},
		{"a Go keyword", "[x]\nother = \"{{.type}}\"", `field "type" is taken`},
		{"a Go type", "[x]\nother = \"{{.string}}\"", `field "string" is taken`},
		{"count without plurals", "[x]\nother = \"{{.count}}\"", "add a one text"},
		{"same Go name", "[no_vm]\nother = \"x\"\n[no_v_m]\nother = \"y\"", "both NoVM"},
	} {
		_, err := generate([]byte(tt.src))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", tt.name, err, tt.want)
		}
	}
}
