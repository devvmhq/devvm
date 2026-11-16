package proxy

import (
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The child's confinement as it starts: its confinement (IO.confine) gets how the broker started
// it, the folders of its logs and the Nix cache and the file to refuse it, and its answer is how
// the child is confined, once the host's addresses still read. No confinement, one that fails, or
// a host whose addresses can't be read then, fails the start.
func TestConfineStarted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w := &wireStart{
		Spawned: unconfined(WhyUnsupported).String(), Log: filepath.Join(dir, "logs", "network.jsonl"),
		ProxyLog: filepath.Join(dir, "logs", "proxy.log"), NixCache: filepath.Join(dir, "nix"),
		Files: []wireWatch{{Kind: watchPolicy, Path: filepath.Join(dir, "policy")}},
	}
	server := func(as ...net.Addr) *server {
		return &server{egress: &egress{own: &hostAddrs{read: func() ([]net.Addr, error) { return as, nil }}}}
	}
	addr := &net.IPNet{IP: net.IPv4(192, 0, 2, 1), Mask: net.CIDRMask(24, 32)}
	full := Confinement{Level: ConfinedFull, By: ByLandlock}
	var got ChildNeeds
	var probed string
	conf := func(spawned Confinement, n ChildNeeds, probe string) (Confinement, error) {
		if spawned != unconfined(WhyUnsupported) {
			t.Errorf("spawned %+v", spawned)
		}
		got, probed = n, probe
		return full, nil
	}
	c, err := confineStarted(w, server(addr), conf, "/run/proxy.json")
	if err != nil || c != full {
		t.Fatalf("confined: %+v, %v", c, err)
	}
	if !slices.Equal(got.Folders, []string{filepath.Join(dir, "logs"), filepath.Join(dir, "nix")}) ||
		!slices.Equal(got.Watched, []string{filepath.Join(dir, "policy")}) || probed != "/run/proxy.json" {
		t.Errorf("needs %+v, probe %q", got, probed)
	}
	if _, err := confineStarted(w, server(), conf, ""); err == nil || !strings.Contains(err.Error(), "addresses") {
		t.Errorf("no addresses: %v", err)
	}
	if _, err := confineStarted(w, server(addr), nil, ""); err == nil {
		t.Error("no confinement: no error")
	}
	failing := func(Confinement, ChildNeeds, string) (Confinement, error) {
		return Confinement{}, errors.New("refused")
	}
	if _, err := confineStarted(w, server(addr), failing, ""); err == nil {
		t.Error("a failed confinement: no error")
	}
	bad := *w
	bad.Spawned = "jailed"
	if _, err := confineStarted(&bad, server(addr), conf, ""); err == nil {
		t.Error("a start of no known confinement: no error")
	}
}
