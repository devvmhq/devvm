package cli

import (
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/image"
)

func TestJoined(t *testing.T) {
	names := []string{"base", "docker", "nix", "foo", "fop"}
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", names},
		{"d", []string{"docker"}},
		{"docker+", []string{"docker+base", "docker+nix", "docker+foo", "docker+fop"}},
		{"docker+f", []string{"docker+foo", "docker+fop"}},
		{"docker+foo+f", []string{"docker+foo+fop"}},
		{"x", nil},
	} {
		if got := joined(tc.in, names); !slices.Equal(got, tc.want) {
			t.Errorf("joined(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --tools, --agents and --layers complete a comma-separated list of their
// part's names, and none.
func TestCompleteParts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	u, _, _ := testUI()
	a := newApp(u)
	for _, tc := range []struct {
		flag, in string
		want     []string
	}{
		{"tools", "", []string{"node", "python", "rust", "go", "dotnet", "none"}},
		{"tools", "node,p", []string{"node,python"}},
		{"agents", "c", []string{"claude", "codex"}},
		{"agents", "n", []string{"none"}},
		{"layers", "", []string{"desktop", "docker", "nix", "none"}},
		{"layers", "de", []string{"desktop"}},
	} {
		if got, _ := completeParts(a, tc.flag)(nil, nil, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("--%s %q = %q, want %q", tc.flag, tc.in, got, tc.want)
		}
	}
}

// devvm tools add and rm complete separate words: the shared tools not
// given yet.
func TestCompleteToolNames(t *testing.T) {
	for _, tc := range []struct {
		args []string
		in   string
		want []string
	}{
		{nil, "c", []string{"claude", "codex"}},
		{[]string{"claude"}, "c", []string{"codex"}},
		{[]string{"node"}, "", slices.Concat([]string{"python", "rust", "go", "dotnet"}, image.Agents)},
		{nil, "node+", nil},
	} {
		if got, _ := completeToolNames(nil, tc.args, tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("completeToolNames(%q, %q) = %q, want %q", tc.args, tc.in, got, tc.want)
		}
	}
}
