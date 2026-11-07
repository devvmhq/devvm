package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/msg"
)

// Repo is devvm's repository, whose GitHub releases devvm upgrade
// installs from, as install.sh does.
const Repo = "https://github.com/devvmhq/devvm"

// Caps on what a release's server sends before it's checked.
const (
	sumsCap      = 64 << 10
	signatureCap = 4 << 10
	// archiveCap is an archive's: devvm's binary and three text files
	// are far less.
	archiveCap = 256 << 20
	// timeout is each request's, a download's body included.
	timeout = 10 * time.Minute
)

// A Source is where releases come from: a repository's GitHub releases,
// at URL (Repo, or a test's server), over Client, which follows redirects
// over HTTPS only. Key is the release key's public half, which signs
// each release's SHA256SUMS; "" when this devvm has none.
type Source struct {
	URL    string
	Client *fetch.Client
	Key    string
}

// NewSource returns the source at url, Repo when it's "", with key, over
// an HTTPS-only client.
func NewSource(url, key string) Source {
	if url == "" {
		url = Repo
	}
	return Source{URL: strings.TrimSuffix(url, "/"), Client: fetch.NewClient(timeout, fetch.HTTPSOnly), Key: key}
}

// Latest returns the newest release's version, from where the
// repository's releases/latest goes, as install.sh reads it: no API, so
// no rate limit. GitHub's releases/latest skips prereleases, and a
// prerelease there anyway is refused, so it's never installed or
// announced as the newest release.
func (s Source) Latest(ctx context.Context) (string, error) {
	end, err := fetch.Resolve(ctx, s.Client, s.URL+"/releases/latest")
	if err != nil {
		return "", msg.UpgradeNoLatest(s.URL).Because(err)
	}
	_, v, ok := strings.Cut(end, "/releases/tag/v")
	if !ok || !Valid(v) {
		return "", msg.UpgradeNoLatest(s.URL)
	}
	if Prerelease(v) {
		return "", msg.UpgradeNoLatest(s.URL).Because(errors.New("releases/latest is the prerelease " + v))
	}
	return v, nil
}

// Archive is the name of version's archive for the platform this devvm
// runs on, as .goreleaser.yaml names it, without .tar.gz.
func Archive(version string) string {
	return "devvm-" + version + "-" + runtime.GOOS + "-" + runtime.GOARCH
}

// Fetch downloads version's archive for this platform into dir and
// returns its path, once it's checked: SHA256SUMS's signature first, by
// the release key, its trusted comment naming version as
// scripts/release.sh writes it, then the archive's SHA-256 against its
// line in SHA256SUMS. Nothing unchecked is left in dir.
func (s Source) Fetch(ctx context.Context, version, dir string) (string, error) {
	if s.Key == "" {
		return "", msg.UpgradeNoKey()
	}
	key, err := ParsePublicKey(s.Key)
	if err != nil {
		return "", msg.UpgradeNoKey().Because(err)
	}
	base := s.URL + "/releases/download/v" + version
	sums, err := fetch.Get(ctx, s.Client, base+"/SHA256SUMS", sumsCap)
	if err != nil {
		return "", msg.UpgradeDownloadFailed("SHA256SUMS", version).Because(err)
	}
	sig, err := fetch.Get(ctx, s.Client, base+"/SHA256SUMS.minisig", signatureCap)
	if err != nil {
		return "", msg.UpgradeDownloadFailed("SHA256SUMS.minisig", version).Because(err)
	}
	trusted, err := key.Verify(sums, sig)
	if err != nil {
		return "", msg.UpgradeBadSignature(version).Because(err)
	}
	if trusted != "devvm "+version {
		return "", msg.UpgradeBadSignature(version).Because(errors.New("its trusted comment is " + trusted))
	}
	name := Archive(version) + ".tar.gz"
	want, ok := sumOf(sums, name)
	if !ok {
		return "", msg.UpgradeNoArchive(name, version)
	}
	path := filepath.Join(dir, name)
	if err := fetch.Download(ctx, s.Client, base+"/"+name, path, want, archiveCap); err != nil {
		if errors.Is(err, fetch.ErrChecksum) {
			return "", msg.UpgradeBadChecksum(name, version).Because(err)
		}
		return "", msg.UpgradeDownloadFailed(name, version).Because(err)
	}
	return path, nil
}

// sumOf is name's SHA-256 in sums, a SHA256SUMS file: the hash of the
// line whose file is name, written as sha256sum writes it.
func sumOf(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return f[0], true
		}
	}
	return "", false
}
