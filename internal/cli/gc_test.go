package cli

import (
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// What gc deleted is said in the order it did: an image its backend
// refused with the backend's message, on stderr, and nothing for what it
// had none of; the package cache's sets alone still say it.
func TestShowGCDone(t *testing.T) {
	u, out, errOut := testUI()
	img := func(name, key string) vm.ListedImage {
		return vm.ListedImage{Listed: build.Listed{Image: build.Image{Key: key, Name: name}}}
	}
	showGCDone(u, vm.GCDone{
		Forgot: []vmname.VM{"devvm-a-0123456789abcd"}, Sockets: 2,
		Images: []vm.DeletedImage{
			{ListedImage: img("base", "0123456789ab")},
			{ListedImage: img("", "ba9876543210")},
			{ListedImage: img("foo", "aaaaaaaaaaaa"), Refused: true, Why: msg.ImageInUse("aaaaaaaaaaaa", "devvm-p-0123456789abcd")},
		},
		PackagesDeleted: true, History: 1, ToolsNoVM: 3, NixFiles: 4, NixFreed: 2048,
	})
	want := "==> forgot_orphan vm=devvm-a-0123456789abcd\n" +
		"==> deleted_link_sockets count=2\n" +
		"==> deleted_image image=base key=0123456789ab\n" +
		"==> deleted_unfinished_build key=ba9876543210\n" +
		"==> deleted_packages count=0\n" +
		"==> deleted_history count=1\n" +
		"==> deleted_tools_no_vm count=3\n" +
		"==> deleted_nix_cache count=4 size=2.0K\n"
	if out.String() != want {
		t.Errorf("stdout\n%s\nwant\n%s", out, want)
	}
	if !strings.HasPrefix(errOut.String(), "devvm: image_in_use ") {
		t.Errorf("stderr %q, want image_in_use", errOut)
	}
}
