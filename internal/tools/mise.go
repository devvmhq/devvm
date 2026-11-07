package tools

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/trust"
)

// The mise devvm resolves the tools lock with, on the host: pinned, and
// checked against its checksum before it runs.
const miseVersion = "2026.11.3"

var miseSHA256 = map[string]string{
	"macos-arm64": "6d39119ab914c54ab865204dee9337c71b83a7ed1e730f663c2fc3453e38fe9c",
	"macos-x64":   "49f32abd93f54147af0599a1c9583d2092df8e7250c9fe635783a7932c4087ef",
	"linux-arm64": "a9a2a66bbbe87ea9cc849e1f006606ae2b34f3d3ba56a54d5cd5d64b6557d164",
	"linux-x64":   "c2f7e814cb39f60b7f67a0ff36cb805e94128bf2dff09d175309d0f095b8cc3b",
}

const miseURL = "https://github.com/jdx/mise/releases/download/v%[1]s/mise-v%[1]s-%[2]s"

// downloadTimeout bounds mise's download, start to end.
const downloadTimeout = 10 * time.Minute

// downloadRetries is how many times mise's download is tried again after
// a failure that may pass.
const downloadRetries = 3

// miseCap caps mise's download: its binary is far less.
const miseCap = 256 << 20

// newClient is the client mise is downloaded with: redirects to https
// only.
func newClient() *fetch.Client { return fetch.NewClient(downloadTimeout, fetch.HTTPSOnly) }

// HostMise returns the path of the host's mise: a test build's stand-in
// (Mise) when it has one, else devvm's pinned mise, downloaded once and
// checked.
func (t Versions) HostMise(ctx context.Context) (string, error) {
	if t.Mise != "" {
		return t.Mise, nil
	}
	var osName string
	switch t.OS {
	case "darwin":
		osName = "macos"
	case "linux":
		osName = "linux"
	default:
		return "", fmt.Errorf("no mise for %s", t.OS)
	}
	platform := osName + "-" + MiseArch(t.Arch)
	dir := filepath.Join(t.Dirs.Mise(), miseVersion)
	path := filepath.Join(dir, "mise")
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o100 != 0 {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := t.trustCAs(); err != nil {
		return "", err
	}
	stop := t.Status(msg.DownloadingMise(miseVersion))
	defer stop()
	// fetch keeps the download only once its SHA-256 is the pin's, and
	// leaves nothing when it fails; a failure that may pass (no answer,
	// 408, 429, 5xx) is tried again.
	tmp := path + ".tmp"
	defer func() { _ = os.Remove(tmp) }()
	url := fmt.Sprintf(t.miseURL, miseVersion, platform)
	err := fetch.Retry(ctx, downloadRetries, t.RetryDelay, func() (bool, error) {
		err := fetch.Download(ctx, t.Client, url, tmp, t.miseSums[platform], miseCap)
		return fetch.Transient(err), err
	})
	m, isMsg := errors.AsType[msg.M](err)
	switch {
	case errors.Is(err, fetch.ErrChecksum):
		return "", msg.MiseChecksum(miseVersion)
	case errors.Is(err, fetch.ErrProxy) && isMsg:
		// The company proxy's own refusal, such as its password unsaved.
		return "", m
	case errors.As(err, new(x509.UnknownAuthorityError)):
		// A network that inspects TLS, whose CA this machine doesn't have.
		return "", msg.ToolsUntrusted(t.Dirs.TrustedCAs()).Because(err)
	case err != nil && ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return "", msg.MiseDownloadFailed(miseVersion)
	}
	if err := os.Chmod(tmp, 0o700); err != nil { //nolint:gosec // a program: the user runs it
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// miseCmd returns mise running in dir, a folder of devvm's own, with its
// state there too. mise also reads config files in the folders above (a
// shared temporary folder: anyone's): MISE_CEILING_PATHS, which needs the
// physical path, stops it at dir. It goes through config.toml's company
// proxy when devvm's environment sets none (Proxy's Env): its
// environment, never its arguments, holds the proxy's password, as mise
// takes it only in the proxy's URL. It trusts the user's CAs (caEnv), and
// logs to a file in dir too (miseLog).
func (t Versions) miseCmd(ctx context.Context, mise, dir string, args ...string) (*exec.Cmd, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	proxyEnv, err := t.Proxy.Env(ctx)
	if err != nil {
		return nil, err
	}
	caEnv, err := t.caEnv(dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, mise, args...) //nolint:gosec // devvm's own mise
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"MISE_DATA_DIR="+filepath.Join(dir, "data"),
		"MISE_CACHE_DIR="+filepath.Join(dir, "cache"),
		"MISE_STATE_DIR="+filepath.Join(dir, "state"),
		"MISE_CONFIG_DIR="+filepath.Join(dir, "config"),
		"MISE_GLOBAL_CONFIG_FILE="+filepath.Join(dir, "config", "none.toml"),
		"MISE_TRUSTED_CONFIG_PATHS="+dir,
		"MISE_CEILING_PATHS="+filepath.Dir(dir),
		"MISE_LOG_FILE="+filepath.Join(dir, miseLog),
	)
	cmd.Env = append(cmd.Env, proxyEnv...)
	cmd.Env = append(cmd.Env, caEnv...)
	return cmd, nil
}

// miseLog is the file in its folder that the host's mise logs to as well
// (MISE_LOG_FILE), which resolveFailed reads.
const miseLog = "mise.log"

// untrustedCert is what mise's log says of a server's certificate that
// mise doesn't trust: rustls' error, which mise's TLS is on every host.
const untrustedCert = "invalid peer certificate"

// errUntrusted is what tools_untrusted carries, so Lock keeps it.
var errUntrusted = errors.New("a server's certificate isn't trusted")

// caEnv is what makes the host's mise trust the user's CAs (trust.Read)
// as well as this machine's own, in files in dir, mise's own folder.
// MISE_EXTRA_CA_CERTS names their bundle, which mise adds to its roots on
// every platform, but only in its own HTTP client: its Sigstore client,
// which fetches the trust root that its provenance checks need, ignores
// it. That client reads the system's roots as the rest of mise does, from
// files on Linux, so there SSL_CERT_FILE names a bundle of the system's
// and the user's CAs too (trust.CAs.Host), which every client reads.
// macOS's mise asks the Keychain, which no variable adds to. None without
// CAs.
func (t Versions) caEnv(dir string) ([]string, error) {
	cas, err := trust.Read(t.Dirs.TrustedCAs())
	if err != nil || len(cas) == 0 {
		return nil, err
	}
	extra := filepath.Join(dir, "devvm-ca.pem")
	if err := state.WriteFile(extra, cas.Bundle()); err != nil {
		return nil, err
	}
	env := []string{"MISE_EXTRA_CA_CERTS=" + extra}
	if t.OS != "linux" {
		return env, nil
	}
	roots := filepath.Join(dir, "devvm-roots.pem")
	bundle, rootsEnv, err := cas.Host(roots, t.Getenv)
	if err != nil {
		return nil, err
	}
	return append(env, rootsEnv...), state.WriteFile(roots, bundle)
}

// trustCAs makes the client that downloads the host's mise trust the
// user's CAs (trust.Read) as well as this machine's own.
func (t Versions) trustCAs() error {
	cas, err := trust.Read(t.Dirs.TrustedCAs())
	if err != nil || len(cas) == 0 {
		return err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return err
	}
	pool.AppendCertsFromPEM(cas.Bundle())
	t.Client.Trust(pool)
	return nil
}

// Resolve resolves shared-tools/mise.toml to the newest matching releases, for
// every platform VMs run, into out (and out.toml, the mise.toml it's
// from). Every tool, or nothing: offline, or with a tool mise can't
// resolve, it writes nothing and returns tools_resolve_failed
// (tools_untrusted for a certificate mise doesn't trust), with the
// details in the run log. mise's version lists come from its versions
// host (no GitHub rate limit). It returns the lock it wrote.
func (t Versions) Resolve(ctx context.Context, out string) (Lock, error) {
	mise, err := t.HostMise(ctx)
	if err != nil {
		return Lock{}, err
	}
	tmp, err := os.MkdirTemp("", "devvm-tools.")
	if err != nil {
		return Lock{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	config, err := t.config()
	if err != nil {
		return Lock{}, err
	}
	if err := state.WriteFile(filepath.Join(tmp, "mise.toml"), config); err != nil {
		return Lock{}, err
	}
	cmd, err := t.miseCmd(ctx, mise, tmp, "lock", "--platform", strings.Join(t.platforms(), ","))
	if err != nil {
		return Lock{}, err
	}
	if err := t.LoggedCmd(ctx, msg.ResolvingTools(), cmd); err != nil {
		return Lock{}, t.resolveFailed(ctx, tmp)
	}
	lock, err := parseLockFile(filepath.Join(tmp, "mise.lock"))
	if err != nil {
		return Lock{}, t.resolveFailed(ctx, tmp)
	}
	if lock, err = t.withSDKDownloads(ctx, lock); err != nil {
		_, _ = fmt.Fprintln(t.LogWriter(), err)
		return Lock{}, t.resolveFailed(ctx, tmp)
	}
	names, specs := configTools(config)
	for _, n := range names {
		if !lock.Has(n) {
			return Lock{}, t.resolveFailed(ctx, tmp)
		}
	}
	if lock, err = t.stepBack(ctx, mise, lock, names, specs); err != nil {
		return Lock{}, err
	}
	if err := state.WriteFile(out+".toml", config); err != nil {
		return Lock{}, err
	}
	return lock, state.WriteFile(out, lock.Bytes())
}

// resolveFailed is the failure of mise's run in dir: tools_untrusted when
// its log says a server's certificate wasn't trusted, as on a network
// that inspects TLS, else tools_resolve_failed.
func (t Versions) resolveFailed(ctx context.Context, dir string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log, err := os.ReadFile(filepath.Join(dir, miseLog)) //nolint:gosec // mise's log, in devvm's own folder
	if err == nil && bytes.Contains(log, []byte(untrustedCert)) {
		return backend.Logged(msg.ToolsUntrusted(t.Dirs.TrustedCAs()).Because(errUntrusted))
	}
	return backend.Logged(msg.ToolsResolveFailed())
}

// candidates is how many older versions a step back tries.
const candidates = 5

// stepBack gives a tool whose newest entry has no download for some
// platform (a release without its files yet, a tag with none, node's musl
// builds for days after a release) the newest of its next older versions
// that has them, keeping its specifiers so it still matches mise.toml.
// Older versions come from the tool's backend (GitHub releases for aqua
// tools), not mise's versions host, which can list tags with no release.
// The next daily check tries the newest again.
func (t Versions) stepBack(ctx context.Context, mise string, lock Lock, names []string, specs map[string]string) (Lock, error) {
	for _, tool := range names {
		if lock.HasDownload(tool, t.platforms()) {
			continue
		}
		v, backendName := lock.Version(tool), lock.Backend(tool)
		older := t.olderVersions(ctx, mise, backendName, specs[tool], v)
		one, c := t.olderWithDownload(ctx, mise, tool, backendName, older)
		if one == nil {
			t.Warn(msg.ToolNoDownload(tool, v))
			continue
		}
		one = maps.Clone(one)
		delete(one, "specifiers")
		if s, ok := lock.first(tool)["specifiers"]; ok {
			one["specifiers"] = s
		}
		var err error
		if lock, err = lock.withEntry(tool, one); err != nil {
			return Lock{}, err
		}
		t.Warn(msg.ToolSteppedBack(c, tool, v))
	}
	return lock, nil
}

// olderWithDownload returns the entry of the newest of these older
// versions of a tool (newest first) that has a download for every
// platform, and its version: nil when none has.
func (t Versions) olderWithDownload(ctx context.Context, mise, tool, backendName string, older []string) (lockEntry, string) {
	entries, ok := t.lockVersions(ctx, mise, tool, backendName, older)
	if !ok {
		return nil, ""
	}
	for _, c := range older {
		if one := entries.versionEntry(tool, c); hasDownload(one, t.platforms()) {
			return one, c
		}
	}
	return nil, ""
}

// olderVersions lists up to five versions of a tool older than v, newest
// first: those its backend lists before v (all of them when v isn't
// listed), with spec's prefix ("24") unless it's latest.
func (t Versions) olderVersions(ctx context.Context, mise, backendName, spec, v string) []string {
	dir, err := os.MkdirTemp("", "devvm-tools.")
	if err != nil {
		return nil
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := []string{"ls-remote", backendName}
	if spec != "" && spec != "latest" {
		args = append(args, spec)
	}
	cmd, err := t.miseCmd(ctx, mise, dir, args...)
	if err != nil {
		return nil
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var vs []string
	for _, line := range lines(out) {
		if line == v {
			break
		}
		if image.ToolVersion(line) { // it goes into a TOML list
			vs = append(vs, line)
		}
	}
	vs = vs[max(0, len(vs)-candidates):]
	slices.Reverse(vs)
	return vs
}

// lockVersions locks these versions of a tool, all in one lock (mise
// takes a list of versions), from the backend its newest came from (the
// name mise's registry gives it may stand for another), in a folder of
// its own: mise also reads the mise.toml of the folders above. False when
// there are none, or mise fails.
func (t Versions) lockVersions(ctx context.Context, mise, tool, backendName string, versions []string) (Lock, bool) {
	if len(versions) == 0 {
		return Lock{}, false
	}
	dir, err := os.MkdirTemp("", "devvm-tools.")
	if err != nil {
		return Lock{}, false
	}
	defer func() { _ = os.RemoveAll(dir) }()
	list := `"` + strings.Join(versions, `","`) + `"`
	config := []byte("[tools]\n" + tool + " = [" + list + "]\n")
	if backendName != "" {
		config = append(append(aliasTable(map[string]string{tool: backendName}), '\n'), config...)
	}
	if err := state.WriteFile(filepath.Join(dir, "mise.toml"), config); err != nil {
		return Lock{}, false
	}
	cmd, err := t.miseCmd(ctx, mise, dir, "lock", "--platform", strings.Join(t.platforms(), ","))
	if err != nil {
		return Lock{}, false
	}
	_ = t.LoggedCmd(ctx, msg.ResolvingTools(), cmd) // what it locked is checked below
	l, err := parseLockFile(filepath.Join(dir, "mise.lock"))
	if err == nil {
		l, err = t.withSDKDownloads(ctx, l)
	}
	return l, err == nil
}
