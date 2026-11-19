package msg

import (
	"strings"
	"testing"
)

// The warnings for a copied marketplace's credential, which internal/vm's tests check by name:
// each renders whole, its values in place, and links to its page.
func TestMarketplaceMessages(t *testing.T) {
	for _, m := range []M{
		MarketplaceSecret("/c/agents/claude/.claude/plugins/marketplaces/m/.git/config", "m", "remote.origin.url"),
		MarketplaceUnchecked("/c/agents/claude/.claude/plugins/marketplaces/m/.git/config", "m", "bad config line 2"),
	} {
		text := m.Text()
		if strings.Contains(text, "{{") || strings.Contains(text, "<no value>") || !strings.Contains(text, "/.git/config") || m.DocURL() == "" {
			t.Errorf("%s: %q, link %q", m.ID(), text, m.DocURL())
		}
	}
}
