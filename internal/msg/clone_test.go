package msg

import (
	"strings"
	"testing"
)

// The constructors of the --clone copy's messages render each message:
// those that count what they name in both forms, one and other, with
// every value they take.
func TestCloneCountedMessages(t *testing.T) {
	for _, n := range []int{1, 2} {
		for _, m := range []M{
			CloneStashAllKept(n, "/d", `"stash@{1}"`),
			CloneStashOverKept(n, "/d", `"stash@{1000}"`, "1000"),
			CloneTodoMissing(`"1234567"`, n, "/d"),
			CloneStashUntracked("saved/devvm/stash-0", n, `"f"`, "saved/devvm/stash-0-untracked"),
			CloneStashUntrackedIgnored(n, `"f"`, "saved/devvm/stash-0-untracked"),
			CloneUrlsKept(n, "/d", `"odd"`),
			CloneHooksKept(n, "/d", `"pre-push"`),
			CloneFilteredKept(n, "/d", `"secret"`),
			CloneTypechangeKept(n, "/d", `"foo"`),
			CloneHooksPathKept("/d", "/etc/githooks"),
			CloneUrlsUnread(n, "/d", `"odd"`),
			ClonePushUrlsKept(n, "/d", `"mirror"`),
			CloneHooksDirKept("/d", "/d/.git/hooks"),
			CloneStashFetchFailed("stash-0"),
			ClonePartialBlocked("/d", "git.example.com", "origin"),
			ClonePartialUnreachable("/d", "origin", "file:///srv/r.git"),
			CloneFetchPartial("."),
			ClonePartialBlockedNone("/d", "git.example.com", "origin"),
			ClonePartialDenied("/d", "10.0.0.5", "origin"),
			ClonePartialPromisorKept(n, "/d", `"origin"`),
			ClonePartialRefsKept(n, "/d", `"refs/tags/t"`),
			CloneRefNoObject("refs/tags/t"),
			CloneSubmoduleKept("lib"),
			CopyingSubmodule("lib/deep"),
			CloneSubmodulesKept("."),
			CloneSubmoduleUnchecked("lib"),
			CloneLfsKept(n, `"0123456789ab"`, "."),
			CloneLfsBad(n, `"0123456789ab"`, "."),
			CloneLfsUnfetched(n, `"0123456789ab"`, "."),
			CloneLfsFailed("."),
			CloneLfsStorage("."),
			CloneLfsSetupFailed("."),
			CloneLfsLarge(".", "2.5"),
			NoTerminalLfsLarge(".", "2.5"),
		} {
			if text := m.Text(); text == "" || strings.Contains(text, "{{") || strings.Contains(text, "<no value>") {
				t.Errorf("%s (%d): %q", m.ID(), n, text)
			}
		}
	}
}
