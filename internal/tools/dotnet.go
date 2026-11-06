package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/image"
)

// The .NET SDK's downloads. mise locks dotnet's version alone, without a
// download, as its installer runs Microsoft's install script, which
// nothing pins and which checks nothing. So devvm adds each platform's SDK
// archive and its SHA-512 to the lock's dotnet entries from Microsoft's
// release metadata, as mise does for the other tools from their releases'
// checksums, and shared-tools/build.sh checks the archive against the lock.

// dotnetReleases is where Microsoft lists a .NET channel's releases, each
// SDK with its files and their SHA-512: a format taking the channel, such
// as 10.0.
const dotnetReleases = "https://builds.dotnet.microsoft.com/dotnet/release-metadata/%s/releases.json"

// dotnetFiles is where every SDK archive the lock records comes from.
const dotnetFiles = "https://builds.dotnet.microsoft.com/dotnet/"

// releasesCap caps a channel's release list, which is a few MB.
const releasesCap = 32 << 20

var (
	dotnetVersion = regexp.MustCompile(`^([0-9]+\.[0-9]+)\.[0-9]+$`)
	sha512Hex     = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

// dotnetRID is Microsoft's name for a lock's platform: linux-x64-musl is
// linux-musl-x64.
func dotnetRID(platform string) string {
	arch, musl := strings.CutSuffix(strings.TrimPrefix(platform, "linux-"), "-musl")
	if musl {
		return "linux-musl-" + arch
	}
	return "linux-" + arch
}

// dotnetSDK is an SDK release as a channel's releases.json has it.
type dotnetSDK struct {
	Version string `json:"version"`
	Files   []struct {
		Name string `json:"name"`
		RID  string `json:"rid"`
		URL  string `json:"url"`
		Hash string `json:"hash"`
	} `json:"files"`
}

// download is the SDK's archive for a lock's platform and its SHA-512, in
// hex: false when Microsoft lists none, or one that isn't an https URL of
// its own downloads with a SHA-512.
func (s dotnetSDK) download(platform string) (url, sha512 string, ok bool) {
	rid := dotnetRID(platform)
	for _, f := range s.Files {
		h := strings.ToLower(f.Hash)
		if f.RID == rid && f.Name == "dotnet-sdk-"+rid+".tar.gz" && strings.HasPrefix(f.URL, dotnetFiles) &&
			strings.HasSuffix(f.URL, ".tar.gz") && sha512Hex.MatchString(h) {
			return f.URL, h, true
		}
	}
	return "", "", false
}

// dotnetSDKs reads a channel's releases.json: its SDKs, each release's
// own and the others it lists.
func dotnetSDKs(data []byte) ([]dotnetSDK, error) {
	var doc struct {
		Releases []struct {
			SDK  *dotnetSDK  `json:"sdk"`
			SDKs []dotnetSDK `json:"sdks"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []dotnetSDK
	for _, r := range doc.Releases {
		if r.SDK != nil {
			out = append(out, *r.SDK)
		}
		out = append(out, r.SDKs...)
	}
	return out, nil
}

// withSDKDownloads returns the lock with Microsoft's downloads added to
// each entry of a tool build.sh fetches itself (image.OwnDownload) that
// lacks one for some platform: the SDK's archive, for each platform
// Microsoft has one for, and its SHA-512. Entries with all of them stay as
// they are, and so does a lock where all have them. A version Microsoft
// doesn't list keeps none, which the step back sees; a channel's list it
// can't get fails.
func (t Versions) withSDKDownloads(ctx context.Context, lock Lock) (Lock, error) {
	channels := map[string][]dotnetSDK{}
	for _, d := range image.SharedTools {
		for _, tool := range d.Members {
			if !image.OwnDownload(tool) {
				continue
			}
			es := slices.Clone(lock.entries(tool))
			changed := false
			for i, e := range es {
				if hasDownload(e, t.platforms()) {
					continue
				}
				sdk, err := t.dotnetSDK(ctx, channels, str(e, "version"))
				if err != nil {
					return Lock{}, err
				}
				e = maps.Clone(e)
				for _, p := range t.platforms() {
					if url, sum, ok := sdk.download(p); ok {
						e["platforms."+p] = map[string]any{"checksum": "sha512:" + sum, "url": url}
						es[i], changed = e, true
					}
				}
			}
			if !changed {
				continue
			}
			var err error
			if lock, err = lock.withEntries(tool, es); err != nil {
				return Lock{}, err
			}
		}
	}
	return lock, nil
}

// dotnetSDK returns the SDK of a version from its channel's releases.json,
// which it gets once per channel (channels): an empty one when the
// version isn't shaped like an SDK's or the channel doesn't list it.
func (t Versions) dotnetSDK(ctx context.Context, channels map[string][]dotnetSDK, version string) (dotnetSDK, error) {
	m := dotnetVersion.FindStringSubmatch(version)
	if m == nil {
		return dotnetSDK{}, nil
	}
	sdks, ok := channels[m[1]]
	if !ok {
		url := fmt.Sprintf(t.dotnetURL, m[1])
		var data []byte
		err := fetch.Retry(ctx, downloadRetries, t.RetryDelay, func() (bool, error) {
			var err error
			data, err = fetch.Get(ctx, t.Client, url, releasesCap)
			return fetch.Transient(err), err
		})
		if err == nil {
			sdks, err = dotnetSDKs(data)
		}
		if err != nil {
			return dotnetSDK{}, fmt.Errorf("dotnet's releases: %w", err)
		}
		channels[m[1]] = sdks
	}
	for _, s := range sdks {
		if s.Version == version {
			return s, nil
		}
	}
	return dotnetSDK{}, nil
}
