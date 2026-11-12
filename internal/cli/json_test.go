package cli

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vm"
)

// shape is a JSON value's fields, one "path: type" line each, sorted: an
// array's elements are path[], and only its first is read.
func shape(t *testing.T, data []byte) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	var out []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(strings.TrimPrefix(path+"."+k, "."), e)
			}
		case []any:
			if len(x) == 0 {
				out = append(out, path+": empty")
				return
			}
			walk(path+"[]", x[0])
		case string:
			out = append(out, path+": string")
		case float64:
			out = append(out, path+": number")
		case bool:
			out = append(out, path+": bool")
		case nil:
			out = append(out, path+": null")
		}
	}
	walk("", v)
	sort.Strings(out)
	return out
}

// A shapeCase is a command's --json sample and its shape.
type shapeCase struct {
	command string
	v       any
	want    []string
}

// cloneShapes are the shapes of devvm clone's subcommands.
func cloneShapes() []shapeCase {
	return []shapeCase{{
		"clone fetch",
		cloneFetchOutput{Repos: []cloneFetchRepo{{Path: "a", Remote: "devvm", Branches: []string{"x"}}}, Branches: []cloneFetchBranch{{Name: "x", Repos: []string{"a"}}}},
		[]string{"branches[].name: string", "branches[].repos[]: string", "repos[].branches[]: string", "repos[].path: string", "repos[].remote: string"},
	}}
}

// defaultShapes are the shapes of the settings of size, ports and
// mount-cache: size and ports with and without -g, which shows new VMs'
// default.
func defaultShapes() []shapeCase {
	return []shapeCase{
		{
			"size", sizeJSON(vm.SizeShown{Size: backend.Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}, Pending: backend.Size{CPUs: 2, MemoryGiB: 4, DiskGiB: 120}}),
			[]string{
				"cpus: number", "disk_gib: number", "memory_gib: number",
				"pending.cpus: number", "pending.disk_gib: number", "pending.memory_gib: number",
			},
		},
		{
			"size -g",
			sizeJSON(vm.SizeShown{
				Size: backend.Size{CPUs: 4, MemoryGiB: 8}, Global: true,
				Sources: map[string]string{"cpus": "config.toml", "memory": "settings.json", "disk": "built-in"},
			}),
			[]string{"cpus: number", "memory_gib: number", "sources.cpus: string", "sources.disk: string", "sources.memory: string"},
		},
		{"ports", map[string]string{"ports": "all"}, []string{"ports: string"}},
		{"ports -g", defaultOutput{"ports": "all", "source": "built-in"}, []string{"ports: string", "source: string"}},
		{
			"mount-cache",
			mountCacheOutput{Mode: "mmap", Pending: "none"},
			[]string{"chosen: bool", "mode: string", "pending: string"},
		},
	}
}

// pasteShapes is the shape of devvm paste's --json.
func pasteShapes() []shapeCase {
	return []shapeCase{{
		"paste", pasteJSON(vm.PasteSent{VM: "devvm-a", Path: "/tmp/devvm-paste/paste-20261109-120000-0123abcd.png", Kind: "image", Type: "image/png", Bytes: 1, Copied: true}),
		[]string{"bytes: number", "copied: bool", "kind: string", "path: string", "sent: bool", "source: string", "type: string", "vm: string"},
	}, {
		"paste a file", pasteJSON(vm.PasteSent{VM: "devvm-a", Path: "/tmp/devvm-paste/paste-20261109-120000-0123abcd.txt", Kind: "file", Source: "/Users/me/notes.txt", Bytes: 1, Copied: true}),
		[]string{"bytes: number", "copied: bool", "kind: string", "path: string", "sent: bool", "source: string", "type: string", "vm: string"},
	}, {
		"paste a file you said no to", pasteJSON(vm.PasteSent{VM: "devvm-a", Kind: "file", Source: "/Users/me/notes.txt"}),
		[]string{"bytes: number", "copied: bool", "kind: string", "path: string", "sent: bool", "source: string", "type: string", "vm: string"},
	}}
}

// machineShapes are the shapes of This machine's commands.
func machineShapes() []shapeCase {
	yes, fix := true, "brew install lima"
	return []shapeCase{
		{
			"doctor",
			doctorOutput{
				Backend: "lima", Problems: []problemJSON{{Key: "lima", Fix: &fix, Message: "m"}, {Key: "kvm", Message: "m"}},
				Warnings: []string{"w"}, Tips: []string{"t"}, Docker: yes, Features: map[string]map[string]string{"lima": {"ssh": "native"}},
				Release: &releaseJSON{Latest: "0.2.0", Checked: "2026-11-05T00:00:00Z"},
			},
			[]string{
				"backend: string", "docker: bool", "features.lima.ssh: string", "problems[].fix: string", "problems[].key: string",
				"problems[].message: string", "release.checked: string", "release.latest: string", "tips[]: string", "warnings[]: string",
			},
		},
		{
			"upgrade",
			upgradeJSON{Current: "0.1.0", Latest: "0.2.0", Newer: true, Install: "install.sh", Path: "/l/devvm"},
			[]string{"current: string", "install: string", "latest: string", "newer: bool", "path: string"},
		},
	}
}

// Each command's --json shape: its field names and types, from a sample
// with every field set. A field is only ever added: a change to one that
// fails here is a new jsonVersion. Every command that takes --json has
// one.
func TestJSONShapes(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cases := []shapeCase{
		{
			"ls", lsJSON([]vm.Listed{{
				Name: "devvm-a", Status: "Running", Image: "base", Tools: []string{"node"}, From: "node:24", Git: "on", Net: "internet", Learning: at,
				Backend: "lima", Folder: "/a",
			}}),
			[]string{
				"vms[].backend: string", "vms[].folder: string", "vms[].folder_state: string", "vms[].from: string", "vms[].git: string", "vms[].image: string", "vms[].links: number",
				"vms[].learning_since: string", "vms[].name: string", "vms[].net: string", "vms[].status: string", "vms[].tools[]: string",
			},
		},
		{
			"images", imagesJSON(
				[]vm.ListedImage{{Listed: build.Listed{Image: build.Image{Key: "k", Name: "base"}, SHA256: "ab", State: "current", Origin: "ubuntu 26.04", Size: 1, Sized: true}, Backend: "lima"}},
				[]tools.Listed{{
					Name: "node", Tool: "node", Version: "24", Built: true, Size: 2, VMs: 1, State: "current",
					Backend: "lima", Target: tools.Target{Libc: "gnu"}, Targeted: true,
				}},
				nixCacheJSON{Files: 1, Size: 2, Cap: 3}),
			[]string{
				"images[].backend: string", "images[].digest: string", "images[].from: string", "images[].key: string", "images[].name: string",
				"images[].size: number", "images[].sized: bool", "images[].state: string",
				"nix_cache.cap: number", "nix_cache.files: number", "nix_cache.size: number",
				"tools[].backend: string", "tools[].built: bool", "tools[].libc: string", "tools[].name: string", "tools[].size: number",
				"tools[].state: string", "tools[].tool: string", "tools[].version: string", "tools[].vms: number",
				"total: number", "unused_tools: number",
			},
		},
		{
			"tools", toolsJSON([]tools.VMTool{{Tool: "node", Version: "24.1", Newer: "24.2", Built: true}}),
			[]string{"tools[].newer: string", "tools[].newer_built: bool", "tools[].tool: string", "tools[].version: string"},
		},
		{
			"network",
			// An entry has builtin or file: both set here, for every field.
			networkOutput{Policy: "internet", LearningSince: "t", Allowlist: []policyFileJSON{{
				Builtin: "layers/base.policy", File: "/p", DeniesOnly: true, Rules: []string{"a.test"}, NotRules: []notRuleJSON{{Line: 2, Text: "a b"}},
			}}, Transparent: transparentJSON{On: true, From: "default"}},
			[]string{
				"allowlist[].builtin: string", "allowlist[].denies_only: bool", "allowlist[].file: string", "allowlist[].not_rules[].line: number",
				"allowlist[].not_rules[].text: string", "allowlist[].rules[]: string", "learning_since: string", "policy: string", "transparent.from: string", "transparent.on: bool",
			},
		},
		{"network transparent", transparentJSON{On: false, From: "vm"}, []string{"from: string", "on: bool"}},
		{
			"network lock",
			lockJSON(netpolicy.LearnedHosts{Since: at, Partial: true, Hosts: []netpolicy.Learned{{
				Rule: "h:8080", Host: "h", Port: 8080, Count: 2, Last: "t", Agent: "curl/8.5.0", Tickable: true, Risk: netpolicy.RiskPaste, NoProxy: true,
			}}}),
			[]string{
				"hosts[].agent: string", "hosts[].count: number", "hosts[].host: string", "hosts[].last: string",
				"hosts[].port: number", "hosts[].risk: string", "hosts[].rule: string", "hosts[].tickable: bool", "hosts[].via: string", "learning_since: string", "partial: bool",
			},
		},
		{"network -g", defaultOutput{"policy": "allowlist", "source": "config.toml"}, []string{"policy: string", "source: string"}},
		{
			"network log",
			logOutput{Events: []logEventJSON{{
				Time: "t", Event: "forward", Host: "h", Port: 443, Method: "GET", Path: "/",
				Rule: "h", Reason: "r", Agent: "claude", Secrets: []string{"github"}, Refused: true, Upstream: "proxy:3128", Target: "devvm-db", Via: "dns",
			}}},
			[]string{
				"events[].agent: string", "events[].event: string", "events[].host: string", "events[].method: string", "events[].path: string",
				"events[].port: number", "events[].reason: string", "events[].refused: bool", "events[].rule: string", "events[].secrets[]: string",
				"events[].target: string", "events[].time: string", "events[].upstream: string", "events[].via: string",
			},
		},
		{
			"network log --blocked",
			blockedOutput{Blocked: []blockJSON{blockJSON(netpolicy.Block{Last: "t", Count: 2, Host: "h", Port: 443, Allow: "h"})}},
			[]string{"blocked[].allow: string", "blocked[].count: number", "blocked[].host: string", "blocked[].last: string", "blocked[].port: number"},
		},
		{
			"ssh-agent",
			sshAgentOutput{Mode: "devvm", Keys: []agentKeyJSON{agentKeyJSON(sshagent.ListedKey{File: "/k", Fingerprint: "SHA256:x", Confirm: true, Lifetime: "12h", Loaded: true})}},
			[]string{"keys[].confirm: bool", "keys[].file: string", "keys[].fingerprint: string", "keys[].lifetime: string", "keys[].loaded: bool", "mode: string"},
		},
		{"git-protect", map[string]string{"mode": "on"}, []string{"mode: string"}},
		{"clone", map[string]string{"clone": "on"}, []string{"clone: string"}},
		{
			"secret",
			secretOutput{Secrets: []secretJSON{{Name: "stripe", Saved: true, UsedBy: []string{"devvm-a"}, Custom: true, Hosts: []string{"api.stripe.com"}, Header: "Authorization", Env: "STRIPE_API_KEY", Prefix: "sk_", Scheme: "ApiKey", Path: "/v1/", BasicUser: true}}},
			[]string{"secrets[].name: string", "secrets[].saved: bool", "secrets[].used_by[]: string", "secrets[].custom: bool", "secrets[].hosts[]: string", "secrets[].header: string", "secrets[].env: string", "secrets[].prefix: string", "secrets[].scheme: string", "secrets[].path: string", "secrets[].basic_user: bool"},
		},
		{
			"link", linkJSON([]vm.LinkListed{{Name: "db", Port: 5432, Target: "devvm-db-0123456789abcd", Folder: "/db", Status: "Running", Reason: vm.LinkUnpublished}}),
			[]string{
				"links[].applies: bool", "links[].name: string", "links[].port: number", "links[].reason: string", "links[].target: string",
				"links[].target_folder: string", "links[].target_status: string",
			},
		},
		{"link publish", map[string]string{"ports": "5432,8000-8010"}, []string{"ports: string"}},
		{
			"group", groupJSON(vm.GroupShown{Group: "web", Members: []vm.GroupMember{{Name: "db", VM: "devvm-db-0123456789abcd", Folder: "/db", Published: "5432"}}}),
			[]string{"group: string", "members[].folder: string", "members[].name: string", "members[].published: string", "members[].vm: string"},
		},
		{
			"history", historyJSON([]vm.Export{{Path: "/e.tar.gz", VM: "devvm-a", Folder: "/a", At: at, Size: 3}}),
			[]string{"exports[].date: string", "exports[].file: string", "exports[].folder: string", "exports[].size: number", "exports[].vm: string"},
		},
		{"space", spaceReport{
			Rows:  []spaceRow{{What: "VMs", Backend: "lima", Count: 1, Size: 1, Sized: true, Kept: keptOnlyCopy, GC: 1, Rest: "devvm rm", Retain: "30d", RetainFrees: 1}},
			Total: 1, GC: 1,
			Caches:  []cacheRow{{Backend: "lima", Name: "images", Path: "/c", Count: 1, Size: 1, Sized: true}},
			Unknown: []string{"qemu"}, Strays: []strayVM{{Backend: "lima", Name: "devvm-b", Data: "/d"}},
			FolderGone:   []goneVM{{Backend: "lima", Name: "devvm-c", Folder: "/c"}},
			NotConnected: []goneVM{{Backend: "lima", Name: "devvm-d", Folder: "/d"}},
		}, []string{
			"folder_gone[].backend: string", "folder_gone[].folder: string", "folder_gone[].name: string",
			"folder_not_connected[].backend: string", "folder_not_connected[].folder: string", "folder_not_connected[].name: string",
			"backends[].backend: string", "backends[].count: number", "backends[].name: string", "backends[].path: string",
			"backends[].size: number", "backends[].sized: bool",
			"devvm[].backend: string", "devvm[].count: number", "devvm[].gc: number", "devvm[].kept: string", "devvm[].rest: string",
			"devvm[].retain: string", "devvm[].retain_frees: number", "devvm[].size: number", "devvm[].sized: bool", "devvm[].what: string",
			"gc: number", "strays[].backend: string", "strays[].data: string", "strays[].name: string", "total: number", "unknown[]: string",
		}},
		{"retention", retentionJSON(settings.Retention{}), retentionShape()},
	}
	cases = append(cases, defaultShapes()...)
	cases = append(cases, machineShapes()...)
	cases = append(cases, cloneShapes()...)
	cases = append(cases, pasteShapes()...)
	shaped := map[string]bool{}
	for _, tc := range cases {
		path, _, _ := strings.Cut(tc.command, " -")
		shaped["devvm "+path] = true
		t.Run(tc.command, func(t *testing.T) {
			u, out, _ := testUI()
			if err := printJSON(u, tc.v); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"version: number"}, tc.want...)
			sort.Strings(want)
			if got := shape(t, out.Bytes()); !slices.Equal(got, want) {
				t.Errorf("devvm %s --json:\n%s\nwant:\n%s", tc.command, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
	u, _, _ := testUI()
	eachCommand(NewRoot(u), func(c *cobra.Command) {
		if c.LocalFlags().Lookup("json") != nil && !shaped[c.CommandPath()] {
			t.Errorf("%s --json has no shape here", c.CommandPath())
		}
	})
}

// Every command that shows or lists something takes --json: each one
// users see takes it, or its about says why not: what it does shows or
// lists nothing.
func TestShowCommandsTakeJSON(t *testing.T) {
	u, _, _ := testUI()
	root := NewRoot(u)
	root.InitDefaultHelpCmd()
	eachCommand(root, func(c *cobra.Command) {
		path := c.CommandPath()
		_, without := c.Annotations[noJSONKey]
		switch takes := c.LocalFlags().Lookup("json") != nil; {
		case c.Hidden && without:
			t.Errorf("%s is hidden: take noJSON out of its about", path)
		case c.Hidden:
		case takes && without:
			t.Errorf("%s takes --json: take noJSON out of its about", path)
		case !takes && !without:
			t.Errorf("%s takes no --json: give it one, or say why not in its about's noJSON", path)
		}
	})
}

// retentionShape is every retention setting's key, a string each, in
// settings and in sources.
func retentionShape() []string {
	var out []string
	for _, field := range []string{"settings", "sources"} {
		for _, k := range settings.RetentionKeys() {
			out = append(out, field+"."+k+": string")
		}
	}
	return out
}

// printJSON writes one indented object, "version" first, HTML characters
// as they are; nothing but an object.
func TestPrintJSON(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
	}{
		{struct{}{}, "{\n  \"version\": 1\n}\n"},
		{map[string]string{"a": "<b>&"}, "{\n  \"version\": 1,\n  \"a\": \"<b>&\"\n}\n"},
	} {
		u, out, _ := testUI()
		if err := printJSON(u, tc.v); err != nil || out.String() != tc.want {
			t.Errorf("printJSON(%v) = %q, %v; want %q", tc.v, out, err, tc.want)
		}
	}
	u, out, _ := testUI()
	if err := printJSON(u, []string{"a"}); err == nil || out.Len() != 0 {
		t.Errorf("an array: %q, %v", out, err)
	}
}
