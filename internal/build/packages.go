package build

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ubuntu"
)

// Base's packages, from the backend's own Ubuntu (assets/layers/apt.sh
// runs in its build). A base key's first build records them: apt gets
// them from Ubuntu's archive, the mirror config.toml sets tried first,
// checking each against the archive's signature, and the build gives back
// each package it downloaded with apt's entry for it, and the InRelease
// files and package lists apt used. devvm checks that what comes back is
// what it says it is: each InRelease signed by Ubuntu's archive key,
// pinned in devvm's own files, each list one a signed InRelease gives the
// SHA-256 of, and each package one a list gives the SHA-256 of, whose
// entry there is the one kept, and the newest version that the release
// pocket's, -updates' and -security's lists, all of which must come
// back, have of it. It keeps each file in its cache
// under its SHA-256 and the set under the key. Every later build of that
// key replays the set: the build VM gets those files from the cache and
// installs from them alone, so it needs no network for them, and the same
// key always means the same packages. A --from base gets neither: its
// image's own sources, unpinned.

// The most a build may give back: a set's list of entries, one package
// file, all of them, and how many entries its tar stream holds; one
// InRelease, one of the archive's package lists and all of those lists.
const (
	maxPackagesList    = 64 << 20
	maxPackageFile     = 2 << 30
	maxPackagesAll     = 16 << 30
	maxPackagesEntries = 20000
	maxInRelease       = 4 << 20
	maxInReleases      = 32 << 20
	maxArchiveList     = 512 << 20
	maxArchiveLists    = 2 << 30
	maxArchiveLine     = 1 << 20
	maxArchiveEntry    = 4 << 20
)

var (
	debName = regexp.MustCompile(`^[0-9a-f]{64}\.deb$`)
	// listName is an InRelease or a package list apt.sh gave back, which
	// it numbers.
	listName = regexp.MustCompile(`^[0-9]{1,4}\.(InRelease|Packages)$`)
)

// packages is how a build of base with key gets its packages: replay, from
// the set key recorded; else record, whose taken is filled once the build
// gives them back and stored by its commit. nil for an image that isn't a
// base from the backend's own Ubuntu.
type packages struct {
	spec  *backend.Packages
	taken *taken // recording: what the build gave back; nil replaying
}

// cache is the package cache in devvm's folders, and devvm's own files,
// which hold Ubuntu's archive key (ubuntu.ArchiveKeyring).
type cache struct {
	paths.Dirs
	Assets fs.FS
}

// basePackages returns base's packages for the build of key, from the
// backend's own Ubuntu: the set key recorded, checked against the cache
// (packages_missing for a file that's gone or changed), or a recording.
func (b cache) basePackages(key string) (*packages, error) {
	set, err := state.ReadPackageSet(b.Dirs, key)
	switch {
	case err == nil:
		if err := b.checkCached(set); err != nil {
			return nil, err
		}
		return &packages{spec: &backend.Packages{Send: func(w io.Writer) error { return b.sendSet(w, set) }}}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	mirror, err := settings.UbuntuMirror(b.Dirs)
	if err != nil {
		return nil, err
	}
	t := &taken{dirs: b, mirror: mirror}
	return &packages{
		spec: &backend.Packages{
			Send: func(w io.Writer) error { return sendMirrors(w, mirror) },
			Take: t.take,
		},
		taken: t,
	}, nil
}

// checkCached checks that every package of set is in the cache with its
// SHA-256, before anything is built.
func (b cache) checkCached(set state.PackageSet) error {
	for _, p := range set.Packages {
		if sum, size, err := hashFile(b.PackageFile(p.SHA256)); err != nil || sum != p.SHA256 || size != p.Size {
			return msg.PackagesMissing(p.Name, p.Version)
		}
	}
	return nil
}

func hashFile(name string) (sum string, size int64, err error) {
	f, err := os.Open(name) //nolint:gosec // a path from paths.Dirs
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// sendMirrors is a recording's input: in/mirrors, the mirror to try first
// ("" for none), which apt.sh puts before each of Ubuntu's archives.
func sendMirrors(w io.Writer, mirror string) error {
	tw := tar.NewWriter(w)
	data := ""
	if mirror != "" {
		data = mirror + "\n"
	}
	if err := writeTarFile(tw, "in/mirrors", []byte(data)); err != nil {
		return err
	}
	return tw.Close()
}

// sendSet is a replay's input: set's package files, from the cache, and
// in/Packages, their entries, each naming its file.
func (b cache) sendSet(w io.Writer, set state.PackageSet) error {
	tw := tar.NewWriter(w)
	if err := writeTarFile(tw, "in/Packages", replayList(set)); err != nil {
		return err
	}
	for _, p := range set.Packages {
		if err := b.sendCached(tw, p); err != nil {
			return err
		}
	}
	return tw.Close()
}

// sendCached sends p's file from the cache, checked again as it goes.
func (b cache) sendCached(tw *tar.Writer, p state.Package) error {
	f, err := os.Open(b.PackageFile(p.SHA256))
	if err != nil {
		return msg.PackagesMissing(p.Name, p.Version).Because(err)
	}
	defer func() { _ = f.Close() }()
	if err := tw.WriteHeader(&tar.Header{Name: "in/" + p.SHA256 + ".deb", Mode: 0o644, Size: p.Size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, p.Size)); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return msg.PackagesMissing(p.Name, p.Version)
	}
	return nil
}

// replayList is apt's list for a replay: each package's entry as
// recorded, with its file's name in the replay's folder.
func replayList(set state.PackageSet) []byte {
	var out bytes.Buffer
	for _, p := range set.Packages {
		for line := range strings.Lines(p.Entry) {
			if !strings.HasPrefix(line, "Filename:") {
				out.WriteString(strings.TrimSuffix(line, "\n") + "\n")
			}
		}
		out.WriteString("Filename: ./" + p.SHA256 + ".deb\n\n")
	}
	return out.Bytes()
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// taken is what a recording's build gave back: its package files and the
// archive's package lists, kept in a folder of its own in the cache (dir)
// until commit, the InRelease files, and the set they make.
type taken struct {
	dirs        cache
	mirror      string // config.toml's ubuntu-mirror, "" for none
	dir         string
	set         state.PackageSet
	names       []string // every InRelease's and list's, as given back
	releases    [][]byte // the InRelease files, as given back
	releaseSize int64    // all of them
	lists       []string // the package lists' names in dir
	listSize    int64    // all of them
}

// take reads the build's tar stream of out/: Packages, apt's entries for
// what it downloaded, each package's file as <sha256>.deb, and the
// InRelease files and package lists apt used, as <n>.InRelease and
// <n>.Packages. Anything else, a file whose content isn't its name's
// SHA-256, an entry without its file, a file without its entry, or a
// package the archive's signed lists don't give is refused
// (packages_refused), and nothing is kept: the build VM wrote all of it.
func (t *taken) take(r io.Reader) (err error) {
	if err := state.PrivateDir(t.dirs.Packages()); err != nil {
		return err
	}
	if t.dir, err = os.MkdirTemp(t.dirs.Packages(), ".taken-"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			t.discard()
		}
	}()
	var list []byte
	files := map[string]int64{} // by SHA-256: its size
	var total int64
	tr := tar.NewReader(r)
	for n := 0; ; n++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return refused(err.Error())
		}
		// Each entry's header counts too, and there are only so many.
		if total += 512; n >= maxPackagesEntries {
			return refused("more than " + strconv.Itoa(maxPackagesEntries) + " entries")
		}
		name := path.Clean(h.Name)
		switch {
		case name == "." && h.Typeflag == tar.TypeDir:
			continue
		case h.Typeflag != tar.TypeReg:
			return refused("not a file: " + strconv.Quote(h.Name))
		case name == "Packages":
			if list != nil {
				return refused("twice: Packages")
			}
			if list, err = fetch.ReadCapped(tr, maxPackagesList); err != nil {
				return refused("Packages: " + err.Error())
			}
		case debName.MatchString(name):
			sum := strings.TrimSuffix(name, ".deb")
			if _, ok := files[sum]; ok {
				return refused("twice: " + name)
			}
			if total += h.Size; total > maxPackagesAll {
				return refused("more than " + strconv.Itoa(maxPackagesAll>>30) + " GiB")
			}
			if files[sum], err = t.keep(tr, sum); err != nil {
				return err
			}
		case listName.MatchString(name):
			if err := t.keepList(tr, name); err != nil {
				return err
			}
		default:
			return refused("not a package: " + strconv.Quote(h.Name))
		}
	}
	signed, err := t.signedEntries(files)
	if err != nil {
		return err
	}
	if t.set, err = setOf(list, files, signed.entryMap()); err != nil {
		return err
	}
	return signed.checkNewest(t.set)
}

// keepList keeps an InRelease (in memory) or a package list (in the
// folder) the build gave back, up to their caps.
func (t *taken) keepList(r io.Reader, name string) error {
	if slices.Contains(t.names, name) {
		return refused("twice: " + name)
	}
	t.names = append(t.names, name)
	if strings.HasSuffix(name, ".InRelease") {
		b, err := fetch.ReadCapped(r, maxInRelease)
		if err != nil {
			return refused(name + ": " + err.Error())
		}
		if t.releaseSize += int64(len(b)); t.releaseSize > maxInReleases {
			return refused("InRelease files over " + strconv.Itoa(maxInReleases>>20) + " MiB")
		}
		t.releases = append(t.releases, b)
		return nil
	}
	f, err := os.OpenFile(filepath.Join(t.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the folder's own, a checked name
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxArchiveList+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > maxArchiveList:
		return refused(name + " is over " + strconv.Itoa(maxArchiveList>>20) + " MiB")
	}
	if t.listSize += n; t.listSize > maxArchiveLists {
		return refused("package lists over " + strconv.Itoa(maxArchiveLists>>30) + " GiB")
	}
	t.lists = append(t.lists, name)
	return nil
}

// signedEntries is what the package lists the build gave back say of the
// packages of want (by SHA-256: their sizes): each InRelease must check
// out with Ubuntu's archive key, and each list must be one a verified
// InRelease gives the SHA-256 and size of. An InRelease older than the
// pinned cloud image's release is refused too (packages_stale_mirror, or
// packages_stale with no mirror set). resolute-updates' and
// resolute-security's InRelease files must be there, and each verified
// InRelease's lists for the architectures that list the packages, but an
// empty one, so that no newer version can be left out
// (packages_lists_missing). A package no list has is left out, and setOf
// refuses it. Nothing to check for none.
func (t *taken) signedEntries(want map[string]int64) (*signed, error) {
	if len(want) == 0 {
		return nil, nil
	}
	releases, err := t.verified()
	if err != nil {
		return nil, err
	}
	for _, suite := range ubuntu.RequiredSuites() {
		if !slices.ContainsFunc(releases, func(r ubuntu.Release) bool { return r.Suite == suite }) {
			return nil, msg.PackagesListsMissing(suite)
		}
	}
	s := &signed{entries: map[string]string{}, arches: map[string][]string{}, newest: map[string]map[string]newest{}}
	given := make([]map[string]bool, len(releases)) // each release's lists that came back, by path
	for i := range given {
		given[i] = map[string]bool{}
	}
	for _, name := range t.lists {
		// The list's SHA-256 first: only a signed list is read for its
		// entries, so what it holds is the archive's, not the build VM's.
		sum, size, err := hashFile(filepath.Join(t.dir, name))
		if err != nil {
			return nil, err
		}
		var l archiveList
		for i, r := range releases {
			for _, rl := range r.Lists(sum, size) {
				if l.entries == nil {
					if l, err = readArchiveList(filepath.Join(t.dir, name), want); err != nil {
						return nil, err
					}
				}
				given[i][rl.Path] = true
				s.add(l, rl.Arch(), r)
			}
		}
		if l.entries == nil {
			return nil, refused(name + " isn't a package list a signed InRelease gives")
		}
	}
	return s, s.complete(releases, given)
}

// verified is each InRelease the build gave back, its signature checked
// and none older than the pinned cloud image's release.
func (t *taken) verified() ([]ubuntu.Release, error) {
	pin, err := state.ReadUbuntuPin(t.dirs.Dirs)
	if err != nil {
		return nil, err
	}
	var releases []ubuntu.Release
	for _, in := range t.releases {
		r, err := ubuntu.VerifyRelease(t.dirs.Assets, in, time.Now())
		if err != nil {
			return nil, msg.PackagesRefused("an InRelease that Ubuntu's archive key didn't sign").Because(err)
		}
		if r.Stale(pin.Release) {
			return nil, t.stale(r, pin.Release)
		}
		releases = append(releases, r)
	}
	return releases, nil
}

// signed is what the archive's signed lists say of a recording's
// packages: each one's entry, the architectures whose lists have it, and,
// by architecture and package name, the newest version that a pocket apt
// installs from by default lists (the release pocket, resolute-updates or
// resolute-security, not resolute-backports).
type signed struct {
	entries map[string]string            // by SHA-256
	arches  map[string][]string          // by SHA-256
	newest  map[string]map[string]newest // by architecture, then name
}

// newest is a package's newest version in a pocket's list, and the pocket.
type newest struct{ version, suite string }

// add adds what l, one of r's lists for arch, says.
func (s *signed) add(l archiveList, arch string, r ubuntu.Release) {
	for sum, entry := range l.entries {
		if _, ok := s.entries[sum]; !ok {
			s.entries[sum] = entry
		}
		if !slices.Contains(s.arches[sum], arch) {
			s.arches[sum] = append(s.arches[sum], arch)
		}
	}
	if r.Backports() {
		return
	}
	if s.newest[arch] == nil {
		s.newest[arch] = map[string]newest{}
	}
	for name, v := range l.newest {
		if newer(v, s.newest[arch][name].version) {
			s.newest[arch][name] = newest{v, r.Suite}
		}
	}
}

// complete checks, for each architecture that lists the packages, that
// each of ubuntu.RequiredSuites has a verified InRelease that gives lists
// for it, as an InRelease of another archive (archive.ubuntu.com's
// against ports.ubuntu.com's) has none, and that every list each of
// releases gives for it came back (given: by release, the paths that
// did), but an empty one, which lists nothing.
func (s *signed) complete(releases []ubuntu.Release, given []map[string]bool) error {
	var arches []string
	for _, a := range s.arches {
		arches = append(arches, a...)
	}
	slices.Sort(arches)
	for _, arch := range slices.Compact(arches) {
		for _, suite := range ubuntu.RequiredSuites() {
			if !slices.ContainsFunc(releases, func(r ubuntu.Release) bool { return r.Suite == suite && len(r.ListsFor(arch)) > 0 }) {
				return msg.PackagesListsMissing(suite)
			}
		}
		for i, r := range releases {
			for _, l := range r.ListsFor(arch) {
				if l.Size > 0 && !given[i][l.Path] {
					return msg.PackagesListsMissing(r.Suite)
				}
			}
		}
	}
	return nil
}

// entryMap is each package's signed entry, by SHA-256; nil for none.
func (s *signed) entryMap() map[string]string {
	if s == nil {
		return nil
	}
	return s.entries
}

// checkNewest refuses a package of set that a pocket apt installs from by
// default lists a newer version of, for an architecture that lists it
// (packages_not_newest): apt would have installed that one.
func (s *signed) checkNewest(set state.PackageSet) error {
	if s == nil {
		return nil
	}
	for _, p := range set.Packages {
		for _, arch := range s.arches[p.SHA256] {
			n, ok := s.newest[arch][p.Name]
			if !ok {
				continue
			}
			c, err := ubuntu.CompareVersions(n.version, p.Version)
			if err != nil {
				return refused(p.Name + "'s version isn't one: " + strconv.Quote(p.Version))
			}
			if c > 0 {
				return msg.PackagesNotNewest(p.Name, n.version, n.suite, p.Version)
			}
		}
	}
	return nil
}

// newer reports whether version a is newer than b, "" for none; never for
// an a that isn't a version.
func newer(a, b string) bool {
	if b == "" {
		_, err := ubuntu.CompareVersions(a, a)
		return err == nil
	}
	c, err := ubuntu.CompareVersions(a, b)
	return err == nil && c > 0
}

// stale is the refusal of r, an InRelease older than the cloud image
// release: naming the mirror that served it, or Ubuntu's archive when
// none is set.
func (t *taken) stale(r ubuntu.Release, release string) error {
	date := r.Date.Format(time.DateOnly)
	if t.mirror != "" {
		return msg.PackagesStaleMirror(date, t.mirror, release, r.Suite)
	}
	return msg.PackagesStale(date, release, r.Suite)
}

// archiveList is what one of the archive's package lists says: its
// entries for the packages a recording gave back (by SHA-256), and the
// newest version it lists of each package (by name).
type archiveList struct {
	entries map[string]string
	newest  map[string]string
}

// readArchiveList reads one of the archive's package lists, whose SHA-256
// a verified InRelease gives, for the packages of want (by SHA-256).
func readArchiveList(name string, want map[string]int64) (archiveList, error) {
	f, err := os.Open(name) //nolint:gosec // in the build's own folder, a checked name
	if err != nil {
		return archiveList{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxArchiveLine)
	l := archiveList{entries: map[string]string{}, newest: map[string]string{}}
	var entry strings.Builder
	var match, pkg, version string
	end := func() {
		if match != "" && l.entries[match] == "" {
			l.entries[match] = entry.String()
		}
		if pkg != "" && newer(version, l.newest[pkg]) {
			l.newest[pkg] = version
		}
		entry.Reset()
		match, pkg, version = "", "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			end()
			continue
		}
		k, v, _ := strings.Cut(line, ": ")
		switch v = strings.TrimSpace(v); k {
		case "SHA256":
			if _, ok := want[v]; ok {
				match = v
			}
		case "Package":
			pkg = v
		case "Version":
			version = v
		}
		if entry.WriteString(line + "\n"); entry.Len() > maxArchiveEntry {
			return archiveList{}, refused(filepath.Base(name) + " has an entry over " + strconv.Itoa(maxArchiveEntry>>20) + " MiB")
		}
	}
	if err := sc.Err(); err != nil {
		return archiveList{}, refused(filepath.Base(name) + ": " + err.Error())
	}
	end()
	return l, nil
}

// keep writes one package file from r into the folder, checking that its
// content's SHA-256 is sum, and returns its size.
func (t *taken) keep(r io.Reader, sum string) (int64, error) {
	f, err := os.OpenFile(t.dir+"/"+sum+".deb", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the folder's own, a checked name
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxPackageFile+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return 0, err
	case n > maxPackageFile:
		return 0, refused(sum + ".deb is over " + strconv.Itoa(maxPackageFile>>30) + " GiB")
	case hex.EncodeToString(h.Sum(nil)) != sum:
		return 0, refused(sum + ".deb has another SHA-256")
	}
	return n, nil
}

// setOf is the set apt's entries (list) make, each matched with its file
// (files: by SHA-256, its size) and with the archive's signed entry for it
// (signed: by SHA-256), which the set keeps.
func setOf(list []byte, files map[string]int64, signed map[string]string) (state.PackageSet, error) {
	set := state.PackageSet{Recorded: time.Now().UTC().Truncate(time.Second)}
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(strings.TrimSpace(string(list)), "\n\n") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		p, err := packageOf(entry)
		if err != nil {
			return state.PackageSet{}, err
		}
		size, ok := files[p.SHA256]
		switch {
		case !ok:
			return state.PackageSet{}, refused("no file for " + p.Name + " " + p.Version)
		case size != p.Size:
			return state.PackageSet{}, refused(p.Name + " " + p.Version + "'s file isn't the size its entry says")
		case seen[p.SHA256]:
			return state.PackageSet{}, refused("two entries for " + p.Name + " " + p.Version)
		}
		if p, err = archiveEntry(p, signed); err != nil {
			return state.PackageSet{}, err
		}
		seen[p.SHA256] = true
		set.Packages = append(set.Packages, p)
	}
	if len(seen) != len(files) {
		return state.PackageSet{}, refused("a file without an entry")
	}
	return set, nil
}

// archiveEntry is p as the archive's signed lists have it (signed: by
// SHA-256), which must be the same package, version, architecture and
// size: packages_refused for one they don't have.
func archiveEntry(p state.Package, signed map[string]string) (state.Package, error) {
	entry, ok := signed[p.SHA256]
	if !ok {
		return state.Package{}, refused(p.Name + " " + p.Version + "'s SHA-256 isn't in the archive's signed package lists")
	}
	a, err := packageOf(entry)
	if err != nil || a.Name != p.Name || a.Version != p.Version || a.Arch != p.Arch || a.SHA256 != p.SHA256 || a.Size != p.Size {
		return state.Package{}, refused(p.Name + " " + p.Version + "'s entry isn't the archive's")
	}
	return a, nil
}

// packageOf reads one entry of apt's lists: its fields, each a line of
// its own ("Field: value") or a continuation (" value").
func packageOf(entry string) (state.Package, error) {
	p := state.Package{Entry: entry}
	sc := bufio.NewScanner(strings.NewReader(entry))
	sc.Buffer(nil, maxPackagesList)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			k, ok = strings.CutSuffix(line, ":")
		}
		if !ok {
			return state.Package{}, refused("not an entry's line: " + strconv.Quote(line))
		}
		switch k {
		case "Package":
			p.Name = v
		case "Version":
			p.Version = v
		case "Architecture":
			p.Arch = v
		case "SHA256":
			p.SHA256 = v
		case "Size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return state.Package{}, refused("not a size: " + strconv.Quote(v))
			}
			p.Size = n
		}
	}
	if !p.Valid() {
		return state.Package{}, refused("an entry without a package's name, version, architecture, SHA-256 or size")
	}
	return p, nil
}

// commit stores what the build gave back, once the image is built: each
// file in the cache under its SHA-256 (one there already is the same
// file, once its SHA-256 says so; one that changed is replaced, which the
// run log, log, notes), then key's set, under the cache's lock, whose wait
// shows on status's line.
func (t *taken) commit(ctx context.Context, key string, log io.Writer, status func(msg.M) func()) error {
	defer t.discard()
	if err := os.MkdirAll(t.dirs.ImageLocks(), 0o700); err != nil {
		return err
	}
	unlock, err := platform.LockFile(ctx, t.dirs.PackagesLock(), msg.Waiting(status, msg.PackagesWaiting()))
	if err != nil {
		return err
	}
	defer unlock()
	for _, p := range t.set.Packages {
		dst := t.dirs.PackageFile(p.SHA256)
		if _, err := os.Lstat(dst); err == nil {
			if sum, size, err := hashFile(dst); err == nil && sum == p.SHA256 && size == p.Size {
				continue
			}
			_, _ = io.WriteString(log, "package cache: replaced "+dst+" ("+p.Name+" "+p.Version+"), which had changed\n")
		}
		if err := os.Rename(t.dir+"/"+p.SHA256+".deb", dst); err != nil {
			return err
		}
	}
	return state.WritePackageSet(t.dirs.Dirs, key, t.set)
}

// discard deletes what the build gave back that isn't in the cache.
func (t *taken) discard() {
	if t.dir != "" {
		_ = os.RemoveAll(t.dir)
		t.dir = ""
	}
}

func refused(why string) error { return msg.PackagesRefused(why) }

// StalePackages is what of the package cache nothing needs: the sets of
// keys that are neither base's key as it is now nor an image the backend
// has, and the package files that no other set names. Their size is the
// files'. A build's folder of what it gave back (.taken-…) is left alone.
type StalePackages struct {
	Sets  []string // keys
	Files []string // SHA-256s
	Size  int64
}

// Stale returns what of the package cache nothing needs: nothing when the
// backend can't say which images it has.
func (b Images) Stale(ctx context.Context) (StalePackages, error) {
	var s StalePackages
	keys, err := state.PackageSetKeys(b.Dirs)
	if err != nil || len(keys) == 0 {
		return s, err
	}
	have, err := b.Backend.Images(ctx)
	if err != nil {
		return s, nil //nolint:nilerr // a backend that can't list keeps every set: gc says why elsewhere
	}
	current, err := b.Key(image.Base)
	if err != nil {
		return s, err
	}
	used := map[string]bool{}
	for _, k := range keys {
		if k == current || slices.Contains(have, k) {
			set, err := state.ReadPackageSet(b.Dirs, k)
			if err != nil {
				return s, err
			}
			for _, p := range set.Packages {
				used[p.SHA256] = true
			}
			continue
		}
		s.Sets = append(s.Sets, k)
	}
	entries, err := os.ReadDir(b.Dirs.Packages())
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if sum, ok := strings.CutSuffix(e.Name(), ".deb"); ok && debName.MatchString(e.Name()) && !used[sum] {
			s.Files = append(s.Files, sum)
			if fi, err := e.Info(); err == nil {
				s.Size += fi.Size()
			}
		}
	}
	return s, nil
}

// DeletePackages deletes s's sets, then its files, under the cache's lock:
// no build stores a set meanwhile.
func (b Images) DeletePackages(ctx context.Context, s StalePackages) error {
	if err := os.MkdirAll(b.Dirs.ImageLocks(), 0o700); err != nil {
		return err
	}
	unlock, err := platform.LockFile(ctx, b.Dirs.PackagesLock(), msg.Waiting(b.Status, msg.PackagesWaiting()))
	if err != nil {
		return err
	}
	defer unlock()
	for _, k := range s.Sets {
		if err := state.RemovePackageSet(b.Dirs, k); err != nil {
			return err
		}
	}
	// What the sets left name now, under the lock: a build may have
	// recorded one naming a file since s was made.
	used, err := usedFiles(b.Dirs)
	if err != nil {
		return err
	}
	for _, sum := range s.Files {
		if used[sum] {
			continue
		}
		if err := os.Remove(b.Dirs.PackageFile(sum)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return removeOldTaken(b.Dirs)
}

// usedFiles is the package files every set names.
func usedFiles(d paths.Dirs) (map[string]bool, error) {
	keys, err := state.PackageSetKeys(d)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, k := range keys {
		set, err := state.ReadPackageSet(d, k)
		if err != nil {
			return nil, err
		}
		for _, p := range set.Packages {
			used[p.SHA256] = true
		}
	}
	return used, nil
}

// takenAge is how old a build's folder of what it gave back (.taken-…)
// is when gc deletes it: a build that ended without deleting it, as a
// killed devvm does.
const takenAge = 24 * time.Hour

// removeOldTaken deletes builds' folders older than takenAge.
func removeOldTaken(d paths.Dirs) error {
	entries, err := os.ReadDir(d.Packages())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !e.IsDir() || !strings.HasPrefix(e.Name(), ".taken-") || time.Since(fi.ModTime()) < takenAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.Packages(), e.Name())); err != nil {
			return err
		}
	}
	return nil
}
