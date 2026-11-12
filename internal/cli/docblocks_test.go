package cli

import (
	"strings"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/agents"
)

// Each generated block builds from devvm's own files, between markers that name it, and the
// agents' history names every agent's history path, Cursor's chats included.
func TestDocBlocks(t *testing.T) {
	for _, b := range DocBlocks() {
		text, err := b.Text(assets.FS)
		if err != nil || strings.TrimSpace(text) == "" {
			t.Errorf("%s on %s: %q, %v", b.Name, b.Page, text, err)
		}
		if !strings.Contains(b.Begin(), b.Name) || !strings.Contains(b.End(), b.Name) {
			t.Errorf("%s's markers: %s %s", b.Name, b.Begin(), b.End())
		}
	}
	history, err := historyList(assets.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range agents.History() {
		if !strings.Contains(history, "`~/"+p+"`") {
			t.Errorf("the history list has no %s: %s", p, history)
		}
	}
	if !strings.Contains(history, "`~/.cursor/chats`") {
		t.Errorf("the history list has no Cursor chats: %s", history)
	}
}
