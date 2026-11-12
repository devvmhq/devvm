package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// checkedIn is the reference in the repo, from this package's folder.
const checkedIn = "../../../docs/content/docs/cli"

// differences lists the files that differ between dirs a and b, or are
// in only one of them.
func differences(t *testing.T, a, b string) []string {
	t.Helper()
	read := func(dir string) map[string][]byte {
		files := map[string][]byte{}
		names, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			data, err := os.ReadFile(n) //nolint:gosec // the test's own folders
			if err != nil {
				t.Fatal(err)
			}
			files[filepath.Base(n)] = data
		}
		return files
	}
	fa, fb := read(a), read(b)
	var diff []string
	for n, data := range fa {
		if other, ok := fb[n]; !ok || !bytes.Equal(data, other) {
			diff = append(diff, n)
		}
	}
	for n := range fb {
		if _, ok := fa[n]; !ok {
			diff = append(diff, n)
		}
	}
	slices.Sort(diff)
	return diff
}

// generated writes root's reference into a temporary folder.
func generated(t *testing.T, root *cobra.Command) string {
	t.Helper()
	dir := t.TempDir()
	if err := write(root, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReferenceIsCurrent(t *testing.T) {
	if diff := differences(t, generated(t, newRoot()), checkedIn); diff != nil {
		t.Fatalf("docs/content/docs/cli is stale (%v): run mise run cli-docs", diff)
	}
}

func TestStaleReferenceIsCaught(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(root *cobra.Command)
		want []string
	}{
		{"a flag's description", func(root *cobra.Command) {
			build := find(t, root, "build")
			build.InitDefaultHelpFlag()
			build.Flags().Lookup("help").Usage = "edited"
		}, []string{"devvm_build.md"}},
		{"a command's summary", func(root *cobra.Command) {
			find(t, root, "rebuild").Short = "Edited"
		}, []string{"_index.md", "devvm.md", "devvm_rebuild.md"}},
		{"a new command", func(root *cobra.Command) {
			// First of all: no other page's place changes.
			root.AddCommand(&cobra.Command{Use: "aaa", Short: "Aaa", Run: func(*cobra.Command, []string) {}})
		}, []string{"_index.md", "devvm.md", "devvm_aaa.md"}},
		{"a command gone", func(root *cobra.Command) {
			root.RemoveCommand(find(t, root, "adopt"))
		}, []string{"_index.md", "devvm.md", "devvm_adopt.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRoot()
			tc.edit(root)
			if got := differences(t, generated(t, root), checkedIn); !slices.Equal(got, tc.want) {
				t.Errorf("differences = %v, want %v", got, tc.want)
			}
		})
	}
}

// A command's page has no weight, which would depend on its place: Hugo
// sorts the section by title. Only the index has one, the section's place.
func TestPagesHaveNoWeight(t *testing.T) {
	files, err := generate(newRoot())
	if err != nil {
		t.Fatal(err)
	}
	for n, data := range files {
		weight, ok := frontMatter(data)["weight"]
		if n == "_index.md" {
			if weight != "90" {
				t.Errorf("_index.md: weight = %q, want 90", weight)
			}
		} else if ok {
			t.Errorf("%s has weight %s: commands' pages sort by title", n, weight)
		}
	}
}

func TestHiddenCommandsAreLeftOut(t *testing.T) {
	files, err := generate(newRoot())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{
		"_index.md", "devvm.md", "devvm_adopt.md", "devvm_agent.md", "devvm_agent_import.md", "devvm_agent_login.md", "devvm_agent_save.md", "devvm_build.md", "devvm_clone.md", "devvm_clone_add.md", "devvm_clone_fetch.md", "devvm_code.md", "devvm_completion.md", "devvm_desktop.md", "devvm_doctor.md", "devvm_gc.md", "devvm_git-protect.md", "devvm_group.md", "devvm_group_join.md", "devvm_group_leave.md", "devvm_history.md", "devvm_history_export.md", "devvm_history_import.md", "devvm_host.md", "devvm_images.md", "devvm_images_rm.md", "devvm_layer.md", "devvm_layer_add.md", "devvm_layer_new.md", "devvm_layer_rm.md", "devvm_limactl.md",
		"devvm_link.md", "devvm_link_add.md", "devvm_link_publish.md", "devvm_link_rm.md", "devvm_link_unpublish.md", "devvm_ls.md", "devvm_mount-cache.md", "devvm_network.md",
		"devvm_network_allow.md", "devvm_network_deny.md", "devvm_network_lock.md", "devvm_network_log.md", "devvm_network_rm.md", "devvm_network_transparent.md",
		"devvm_paste.md", "devvm_paste_rm.md", "devvm_ports.md", "devvm_ports_add.md", "devvm_ports_rm.md", "devvm_rebuild.md",
		"devvm_restart.md", "devvm_retention.md", "devvm_rm.md", "devvm_secret.md", "devvm_secret_rm.md",
		"devvm_secret_set.md", "devvm_secret_unuse.md", "devvm_secret_use.md",
		"devvm_setup.md", "devvm_size.md", "devvm_space.md", "devvm_ssh-agent.md", "devvm_ssh-agent_add.md", "devvm_ssh-agent_rm.md", "devvm_stop.md",
		"devvm_tools.md", "devvm_tools_add.md", "devvm_tools_rm.md", "devvm_tools_update.md", "devvm_update.md", "devvm_upgrade.md",
	}
	if !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
	for n, data := range files {
		for _, s := range []string{"_msg", "_qemu", "devvm help", "devvm version", "__complete", "Auto generated"} {
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("%s has %q", n, s)
			}
		}
	}
}

func TestWriteReplacesOldPages(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "devvm_gone.md")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := write(newRoot(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("%s is still there (%v)", old, err)
	}
}

func TestTidy(t *testing.T) {
	in := "## devvm x\n\nShort\n\n### Synopsis\n\nLong\n\n```\ndevvm x [flags]\n```\n\n" +
		"### SEE ALSO\n\n* [devvm](devvm.md)\t - Root\n\n"
	want := "Short\n\n## Synopsis\n\nLong\n\n```text\ndevvm x [flags]\n```\n\n" +
		"## See also\n\n- [devvm](devvm.md): Root\n"
	if got := tidy(in); got != want {
		t.Errorf("tidy:\n%s\nwant:\n%s", got, want)
	}
}

func TestWithDocs(t *testing.T) {
	for _, tc := range []struct{ md, doc, want string }{
		{
			"Short\n\n## See also\n\n- x\n", "using/ssh#vs-code",
			"Short\n\n## Docs\n\n[The docs page for devvm x](../using/ssh.md#vs-code) explains it in full.\n\n## See also\n\n- x\n",
		},
		{
			"Short\n", "using/desktop",
			"Short\n\n## Docs\n\n[The docs page for devvm x](../using/desktop.md) explains it in full.\n",
		},
		{"Short\n", "", "Short\n"},
	} {
		if got := withDocs(tc.md, "devvm x", tc.doc); got != tc.want {
			t.Errorf("withDocs(%q, %q):\n%s\nwant:\n%s", tc.md, tc.doc, got, tc.want)
		}
	}
}

// find returns root's subcommand name.
func find(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	c, _, err := root.Find([]string{name})
	if err != nil || c.Name() != name {
		t.Fatalf("no %s command (%v)", name, err)
	}
	return c
}
