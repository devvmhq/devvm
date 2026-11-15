package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every file of a VM's is in its folder, each in its own place, none of
// them where another is.
func TestVMFiles(t *testing.T) {
	v := Dirs{Data: "/d"}.VM("devvm-p-0123456789abcd")
	want := map[string]string{
		v.State():                     "vm.json",
		v.Lock():                      "lock",
		v.Policy():                    "policy",
		v.Run():                       "run",
		v.Logs():                      "logs",
		v.Proxy(".pid"):               "run/proxy.pid",
		v.ProxyConfig():               "run/proxy.json",
		v.Proxy(".status"):            "run/proxy.status",
		v.GitPins(".jsonl"):           "run/git-pins.jsonl",
		v.GitPins(".json"):            "run/git-pins.json",
		v.Profile():                   "run/sandbox.sb",
		v.Proxy(".slots"):             "run/proxy.slots",
		v.BuildProxy():                "run/build-proxy",
		v.Started():                   "run/started",
		v.Secrets():                   "run/secrets.json",
		v.SecretsPolicy():             "run/secrets.policy",
		v.LinksPID():                  "run/links.pid",
		v.LinkList():                  "run/links.json",
		v.EventLog():                  "logs/network.jsonl",
		v.ProxyLog():                  "logs/proxy.log",
		v.LinksLog():                  "logs/links.log",
		v.ForwarderLog():              "logs/ports.log",
		v.KnownHosts():                "run/known_hosts",
		v.Forwarder(".lock"):          "run/ports.lock",
		v.CA().Dir:                    "run/ca",
		v.CA().Cert():                 "run/ca/ca.pem",
		v.CA().Key():                  "run/ca/ca.key",
		v.CA().NextCert():             "run/ca/next.pem",
		v.CA().NextKey():              "run/ca/next.key",
		filepath.Join(v.Dir, "other"): "other",
	}
	if len(want) != 30 {
		t.Fatalf("%d paths, want 30: two are the same", len(want))
	}
	for got, rel := range want {
		if got != filepath.Join("/d/vms/0123456789abcd", rel) {
			t.Errorf("got %s, want %s in the VM's folder", got, rel)
		}
		if strings.HasSuffix(got, ".sock") || strings.HasSuffix(got, ".ctl") {
			t.Errorf("%s: a socket in a VM's folder", got)
		}
	}
}
