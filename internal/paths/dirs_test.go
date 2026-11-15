package paths

import (
	"cmp"
	"path/filepath"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

func TestFromEnv(t *testing.T) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		goos string // linux when empty
		want Dirs
	}{
		{
			name: "defaults under HOME",
			env:  map[string]string{"HOME": "/h"},
			want: Dirs{Home: "/h", Data: "/h/.local/share/devvm", Config: "/h/.config/devvm", Lima: "/h/.local/share/devvm/lima", UserCache: "/h/.cache"},
		},
		{
			name: "HOME cleaned",
			env:  map[string]string{"HOME": "/h//x/../"},
			want: Dirs{Home: "/h", Data: "/h/.local/share/devvm", Config: "/h/.config/devvm", Lima: "/h/.local/share/devvm/lima", UserCache: "/h/.cache"},
		},
		{
			name: "XDG folders, LIMA_HOME ignored",
			env:  map[string]string{"HOME": "/h", "XDG_DATA_HOME": "/d", "XDG_CONFIG_HOME": "/c/", "XDG_CACHE_HOME": "/k", "LIMA_HOME": "/elsewhere"},
			want: Dirs{Home: "/h", Data: "/d/devvm", Config: "/c/devvm", Lima: "/d/devvm/lima", UserCache: "/k"},
		},
		{
			name: "macOS's cache folder, whatever XDG_CACHE_HOME says",
			env:  map[string]string{"HOME": "/h", "XDG_CACHE_HOME": "/k"},
			goos: "darwin",
			want: Dirs{Home: "/h", Data: "/h/.local/share/devvm", Config: "/h/.config/devvm", Lima: "/h/.local/share/devvm/lima", UserCache: "/h/Library/Caches"},
		},
		{
			name: "DEVVM_LIMA_HOME moves the Lima home",
			env:  map[string]string{"HOME": "/h", "XDG_DATA_HOME": "/d", "DEVVM_LIMA_HOME": "/short", "LIMA_HOME": "/elsewhere"},
			want: Dirs{Home: "/h", Data: "/d/devvm", Config: "/h/.config/devvm", Lima: "/short", UserCache: "/h/.cache"},
		},
		{
			name: "a relative DEVVM_LIMA_HOME is made absolute",
			env:  map[string]string{"HOME": "/h", "DEVVM_LIMA_HOME": "lt"},
			want: Dirs{Home: "/h", Data: "/h/.local/share/devvm", Config: "/h/.config/devvm", Lima: filepath.Join(cwd, "lt"), UserCache: "/h/.cache"},
		},
		{
			name: "relative XDG folders are ignored",
			env:  map[string]string{"HOME": "/h", "XDG_DATA_HOME": "d", "XDG_CONFIG_HOME": "c", "XDG_CACHE_HOME": "k"},
			want: Dirs{Home: "/h", Data: "/h/.local/share/devvm", Config: "/h/.config/devvm", Lima: "/h/.local/share/devvm/lima", UserCache: "/h/.cache"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fromEnv(func(k string) string { return tt.env[k] }, cmp.Or(tt.goos, "linux"))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFromEnvNeedsHome(t *testing.T) {
	for _, home := range []string{"", "rel"} {
		_, err := fromEnv(func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		}, "linux")
		if msg.IDOf(err) != "home_not_absolute" {
			t.Errorf("HOME=%q: got %v, want home_not_absolute", home, err)
		}
	}
}

func TestFiles(t *testing.T) {
	d := Dirs{Data: "/d", Config: "/c", Lima: "/l"}
	tests := []struct{ got, want string }{
		{d.VMDir("0123456789abcd"), "/d/vms/0123456789abcd"},
		{d.VM("devvm-p-0123456789abcd").Dir, "/d/vms/0123456789abcd"},
		{d.VM("devvm-a-long-cut-0123456789abcd").Dir, "/d/vms/0123456789abcd"},
		{d.VM("devvm-img-0123456789ab").Dir, "/d/run/builds/devvm-img-0123456789ab"},
		{d.VM("devvm-tools-build").Dir, "/d/run/builds/devvm-tools-build"},
		{d.ToolLockConfig(), "/d/mise.lock.toml"},
		{d.ToolLockChecked(), "/d/mise.lock.checked"},
		{d.ToolsCheckLog(), "/d/logs/tools-check.log"},
		{d.Image("abc"), "/d/images/abc"},
		{d.ImageRecord("abc"), "/d/images/abc/record.json"},
		{d.ImageLocks(), "/d/images/locks"},
		{d.Rebuilds(), "/d/images/rebuilds.json"},
		{d.RebuildsLock(), "/d/images/locks/rebuilds"},
		{d.UbuntuPin(), "/d/images/ubuntu.json"},
		{d.UbuntuPinLock(), "/d/images/locks/ubuntu"},
		{d.Packages(), "/d/packages"},
		{d.PackageFile("ab12"), "/d/packages/ab12.deb"},
		{d.PackageSets(), "/d/packages/sets"},
		{d.PackageSet("0123456789ab"), "/d/packages/sets/0123456789ab.json"},
		{d.PackagesLock(), "/d/images/locks/packages"},
		{d.Cache(), "/d/cache"},
		{d.FromDigests(), "/d/cache/from-digests.json"},
		{d.NixCache(), "/d/cache/nix-cache"},
		{d.ToolRecord("go-1.2.0-0123456789ab.erofs"), "/d/tools/go-1.2.0-0123456789ab.erofs.json"},
		{d.Run(), "/d/run"},
		{d.ClaimsLock(), "/d/run/claims.lock"},
		{d.Logs(), "/d/logs"},
		{d.RunLogs(), "/d/logs/runs"},
		{d.Tools(), "/d/tools"},
		{d.ToolLock(), "/d/mise.lock"},
		{d.Mise(), "/d/cache/mise"},
		{d.Exports(), "/d/exports"},
		{d.AgentSock(), "/d/agent.sock"},
		{d.AgentRelay("devvm-web-0123456789abcd"), "/d/run/agent-0123456789abcd.sock"},
		{d.AgentRelay("devvm-build-x"), "/d/run/agent-devvm-build-x.sock"},
		{d.OCIBuildLock(), "/d/oci-build.lock"},
		{d.LimaVM("devvm-p-1"), "/l/devvm-p-1"},
		{d.ConfigFile(), "/c/config.toml"},
		{d.Settings(), "/d/settings.json"},
		{d.AgentKeys(), "/d/agent-keys.json"},
		{d.Layers(), "/c/layers"},
		{d.Secrets(), "/d/secrets"},
		{d.AgentSettings("claude"), "/c/agents/claude"},
		{d.AgentLogins("codex"), "/d/logins/codex"},
		{d.SSHConfig(), "/d/ssh_config"},
		{d.QEMUWrapper(), "/d/run/qemu"},
		{d.ToolImage("go-1.2.0-0123456789ab.erofs"), "/d/tools/go-1.2.0-0123456789ab.erofs"},
		{d.ShareLock(), "/d/tools.lock"},
		{d.RetentionLock(), "/d/retention.lock"},
		{d.GroupsLock(), "/d/groups.lock"},
		{d.DefaultLima(), "/d/lima"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %s, want %s", tt.got, tt.want)
		}
	}
}
