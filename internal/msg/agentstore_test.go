package msg

import (
	"strings"
	"testing"
)

// devvm agent import's and save's messages about links and the agent store, which their own
// package's tests check by name: each renders whole, its values in place, and links to its page.
func TestAgentStoreMessages(t *testing.T) {
	for _, m := range []M{
		AgentImportLinkNotSetting("/h/.claude/skills/x", "/h/.claude/projects/p"),
		AgentImportReadFailed("/h/.claude/settings.json", "changed"),
		AgentStoreLink("/c/agents/claude"),
	} {
		text := m.Text()
		if strings.Contains(text, "{{") || strings.Contains(text, "<no value>") || !strings.Contains(text, "/") || m.DocURL() == "" {
			t.Errorf("%s: %q, link %q", m.ID(), text, m.DocURL())
		}
	}
}
