package netpolicy

import (
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/proxy"
)

// Risk is why devvm network lock marks a learned host, which starts
// unticked: each is an easy way to send data out. "" for none.
type Risk string

// The risks, as devvm network lock --json names them.
const (
	RiskAddress   Risk = "address"   // an address, not a name
	RiskLookalike Risk = "lookalike" // a punycode label (xn--), which can look like another name
	RiskPaste     Risk = "paste"     // a paste or file drop site
	RiskStorage   Risk = "storage"   // file storage or file sharing
	RiskTunnel    Risk = "tunnel"    // a tunnel to someone's own machine
	RiskWebhook   Risk = "webhook"   // a service that records what it's sent
	RiskHosting   Risk = "hosting"   // free hosting, where anyone runs their own server
	RiskChat      Risk = "chat"      // a chat service's endpoint for bots and webhooks
	RiskMail      Risk = "mail"      // a mail server
)

// riskyHosts are the hosts devvm network lock marks, each matched as
// itself and as any of its subdomains, by why: well known ones of each
// kind, as the docs list their kinds. devvm's own rules leave them out
// (layers/base.policy), and RiskOf adds the patterns a list can't hold.
func riskyHosts() map[Risk][]string {
	return map[Risk][]string{
		RiskPaste: {
			"pastebin.com", "paste.ee", "hastebin.com", "termbin.com", "dpaste.org", "paste.rs", "rentry.co", "ghostbin.site",
			"gist.githubusercontent.com", "gist.github.com", "gofile.io", "pixeldrain.com", "bashupload.com", "uguu.se",
			"filebin.net", "ix.io", "sprunge.us", "bpa.st", "controlc.com", "justpaste.it", "paste.ubuntu.com",
			"rekor.sigstore.dev", // a transparency log, which takes an entry from anyone
		},
		RiskMail: {"l.google.com"}, // Gmail's mail servers, such as aspmx.l.google.com
		RiskStorage: {
			"transfer.sh", "file.io", "0x0.st", "temp.sh", "catbox.moe", "wetransfer.com", "mega.nz", "dropbox.com",
			"dropboxusercontent.com", "drive.google.com", "docs.google.com", "storage.googleapis.com", "firebasestorage.googleapis.com",
			"blob.core.windows.net", "file.core.windows.net", "dfs.core.windows.net", "web.core.windows.net",
			"r2.cloudflarestorage.com", "s3.amazonaws.com", "backblazeb2.com", "digitaloceanspaces.com", "wasabisys.com",
			"linodeobjects.com", "onedrive.live.com", "1drv.ms", "sharepoint.com", "box.com",
		},
		RiskTunnel:  {"ngrok.io", "ngrok.app", "ngrok-free.app", "ngrok.dev", "ngrok-free.dev", "trycloudflare.com", "loca.lt", "serveo.net"},
		RiskWebhook: {"webhook.site", "pipedream.net", "requestcatcher.com", "beeceptor.com", "interact.sh", "burpcollaborator.net"},
		RiskHosting: {
			"r2.dev", "workers.dev", "pages.dev", "vercel.app", "netlify.app", "herokuapp.com", "glitch.me", "repl.co",
			"cloudfront.net", "on.aws", "firebaseio.com", "supabase.co",
		},
		RiskChat: {"discord.com", "discordapp.com", "hooks.slack.com", "api.telegram.org", "script.google.com"},
	}
}

// RiskOf returns why given, a host read as the proxy reads it
// (proxy.NormHost), is risky to allow (Risk): an address; a name
// with a punycode label; a host of riskyHosts or under one; an OAST
// domain (oast.fun, oast.pro and the rest); one of S3's regional hosts,
// AWS's or AWS China's (bucket.s3.us-east-1.amazonaws.com); or an API
// Gateway endpoint (id.execute-api.us-east-1.amazonaws.com). "" for any
// other.
func RiskOf(given string) Risk {
	h := proxy.NormHost(given)
	if _, ok := h.Addr(); ok {
		return RiskAddress
	}
	host := h.String()
	labels := strings.Split(host, ".")
	if slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, "xn--") }) {
		return RiskLookalike
	}
	for why, hosts := range riskyHosts() {
		if slices.ContainsFunc(hosts, h.Under) {
			return why
		}
	}
	n := len(labels)
	aws := strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn")
	switch {
	case n >= 2 && labels[n-2] == "oast":
		return RiskWebhook
	case aws && slices.ContainsFunc(labels, func(l string) bool { return l == "s3" || strings.HasPrefix(l, "s3-") }):
		return RiskStorage
	case aws && slices.Contains(labels, "execute-api"):
		return RiskHosting
	}
	return ""
}

// tickable are the names whose hosts devvm network lock starts ticked, on
// port 443 alone, each matched as itself and as any of its subdomains,
// with why: package registries' and toolchains' and agents' vendors' own
// hosts, reviewed so that nobody but the vendor can make a host or post
// content under them. A registry's packages are its users', but only
// through the registry's own upload, with a token. Nothing else makes a
// host tickable: not your rules, not a secret's hosts, not the VM's own
// policy file. Never add a name where anyone can get a subdomain or a
// page, such as github.io, googleapis.com, amazonaws.com, windows.net or
// pypi.org (upload.pypi.org takes uploads): TestTickable checks a list of
// those.
func tickable() map[string]string {
	return map[string]string{
		"registry.npmjs.org":               "npm's registry: npm alone serves it",
		"registry.yarnpkg.com":             "Yarn's mirror of npm's registry",
		"repo.yarnpkg.com":                 "Yarn's own releases",
		"nodejs.org":                       "Node's releases, the Node project's alone",
		"files.pythonhosted.org":           "PyPI's file host: packages come only through PyPI's upload",
		"crates.io":                        "Rust's registry, its index and its downloads",
		"static.rust-lang.org":             "Rust's own toolchains (not rust-lang.org, whose other hosts aren't only the project's)",
		"proxy.golang.org":                 "Go's module proxy (not golang.org, whose playground shares what anyone writes)",
		"sum.golang.org":                   "Go's checksum database",
		"api.nuget.org":                    "NuGet's registry: packages come only through its upload",
		"builds.dotnet.microsoft.com":      ".NET's own builds (not microsoft.com, whose other hosts take users' content)",
		"releases.astral.sh":               "uv's and ruff's releases",
		"mise-versions.jdx.dev":            "mise's version lists",
		"registry-1.docker.io":             "Docker Hub's registry: images come only through its push",
		"auth.docker.io":                   "Docker Hub's token service",
		"ghcr.io":                          "GitHub's container registry: images come only through its push",
		"cache.nixos.org":                  "Nix's binary cache, which only NixOS's builders fill",
		"api.anthropic.com":                "Anthropic's API (not claude.ai, which shares what users publish)",
		"api.openai.com":                   "OpenAI's API (not chatgpt.com or openai.com, which host what users share)",
		"bun.sh":                           "Bun's own site and installs",
		"production.cloudflare.docker.com": "Docker Hub's image downloads",
	}
}

// Tickable reports whether devvm network lock starts host:port ticked: on
// port 443, and host is one of tickable's names or under one.
func Tickable(host string, port int) bool {
	h := proxy.NormHost(host)
	if port != 443 {
		return false
	}
	for name := range tickable() {
		if h.Under(name) {
			return true
		}
	}
	return false
}
