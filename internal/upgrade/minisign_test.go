package upgrade_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/internal/upgrade/upgradetest"
)

// A test key and signatures that minisign 0.12 made (minisign -G -W, then
// -S -t "devvm 0.2.0", and -S -l for the legacy one), of fixtureData.
const (
	fixturePub = "untrusted comment: minisign public key 44C6F1F3D107DB25\n" +
		"RWQl2wfR8/HGRBjOIm1RjKIjqVhIHEUrlNvdrBt9LF4WGpsLpyP5rtoa\n"
	fixtureData = "abc123  devvm-0.2.0-darwin-arm64.tar.gz\n"
	fixtureSig  = "untrusted comment: signature from minisign secret key\n" +
		"RUQl2wfR8/HGRCXVO0nzTpOil1leGY6nTu17st+xEaHkK8CtJp/j7NdutUNoz7VaWkwNCpuiA0xDeeTIQep8P48BZZNkUsg6pgU=\n" +
		"trusted comment: devvm 0.2.0\n" +
		"xtOEsk6qfbGM/ONm5O54uOVmLc5e9vQUz0FQv1snFDyA4oCjtQ5FTs1um12aGEjvebBU7T6K+7GHpnIDFJyzBg==\n"
	fixtureLegacy = "untrusted comment: signature from minisign secret key\n" +
		"RWQl2wfR8/HGRDFyarxcvUa6k36Nm3B9oKFge5ou69TDttMgD66jqZ4y1aHM26I/2rUEiRGnApFGBe6NBH5e7bd4tY0f9YZXNA8=\n" +
		"trusted comment: devvm 0.2.0\n" +
		"dC6ZE3WFpMqCgdMAgqDpsuLfWDWF1KkX1kofFzs2wsylWF/4W2YiQ/Ld4dW1kDjVfXNTsEa/N6cNp36ph4vgAg==\n"
)

// minisign's own signature verifies, its trusted comment returned, from
// the key's file or its line; anything changed, a legacy signature and
// another key's don't.
func TestVerifyMinisign(t *testing.T) {
	for _, pub := range []string{fixturePub, strings.Split(fixturePub, "\n")[1]} {
		k, err := upgrade.ParsePublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		trusted, err := k.Verify([]byte(fixtureData), []byte(fixtureSig))
		if err != nil || trusted != "devvm 0.2.0" {
			t.Errorf("Verify = %q, %v", trusted, err)
		}
	}
	k, _ := upgrade.ParsePublicKey(fixturePub)
	other, _ := upgrade.ParsePublicKey(upgradetest.NewKey().Public())
	for _, tc := range []struct {
		name      string
		key       upgrade.PublicKey
		data, sig string
	}{
		{"data changed", k, strings.Replace(fixtureData, "abc123", "abc124", 1), fixtureSig},
		{"trusted comment changed", k, fixtureData, strings.Replace(fixtureSig, "devvm 0.2.0", "devvm 9.9.9", 1)},
		{"legacy", k, fixtureData, fixtureLegacy},
		{"another key", other, fixtureData, fixtureSig},
		{"not a signature", k, fixtureData, "nope\n"},
		{"empty", k, fixtureData, ""},
		{"signature cut short", k, fixtureData, strings.Replace(fixtureSig, "RUQl2wfR8/HGRCXV", "RUQl", 1)},
		{"global signature gone", k, fixtureData, strings.Replace(fixtureSig, "xtOEsk6qfbGM", "xtOEsk6qfbGN", 1)},
	} {
		if _, err := tc.key.Verify([]byte(tc.data), []byte(tc.sig)); err == nil {
			t.Errorf("%s: verified", tc.name)
		}
	}
}

// What upgradetest signs, minisign's way, verifies here: the tests' fake
// releases are checked as a real one is.
func TestVerifyTestKey(t *testing.T) {
	key := upgradetest.NewKey()
	k, err := upgrade.ParsePublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.Verify([]byte("x"), key.Sign([]byte("x"), "devvm 1.0.0")); err != nil || got != "devvm 1.0.0" {
		t.Errorf("Verify = %q, %v", got, err)
	}
}

// A public key is minisign's: Ed, a key ID and 32 bytes, alone or as a
// minisign.pub file.
func TestParsePublicKey(t *testing.T) {
	for _, s := range []string{
		"", "nope", "untrusted comment: x\nnope\n", "a\nb\nc",
		"RUQl2wfR8/HGRBjOIm1RjKIjqVhIHEUrlNvdrBt9LF4WGpsLpyP5rtoa", // ED, not Ed
		"RWQl2wfR8/HGRBjOIm1RjKIjqVhIHEUrlNvdrBt9LF4WGpsLpyP5",     // short
	} {
		if _, err := upgrade.ParsePublicKey(s); err == nil {
			t.Errorf("ParsePublicKey(%q) succeeded", s)
		}
	}
}

// FuzzParsePublicKey: a public key is read only from one base64 line, alone or after an
// untrusted comment, that is "Ed", a key ID and an Ed25519 key, and is read as exactly those.
func FuzzParsePublicKey(f *testing.F) {
	for _, s := range []string{
		fixturePub, strings.Split(fixturePub, "\n")[1], upgradetest.NewKey().Public(), "untrusted comment: x\nnope\n", "a\nb\nc",
		"RUQl2wfR8/HGRBjOIm1RjKIjqVhIHEUrlNvdrBt9LF4WGpsLpyP5rtoa", "  RWQl2wfR8/HGRBjOIm1RjKIjqVhIHEUrlNvdrBt9LF4WGpsLpyP5rtoa\r\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		k, err := upgrade.ParsePublicKey(s)
		if err != nil {
			return
		}
		lines := strings.Split(strings.TrimSpace(s), "\n")
		if len(lines) > 2 || len(lines) == 2 && !strings.HasPrefix(lines[0], "untrusted comment: ") {
			t.Fatalf("read a key from %d lines: %q", len(lines), s)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[len(lines)-1]))
		want := append(append([]byte("Ed"), k.ID[:]...), k.Key...)
		if err != nil || len(k.Key) != ed25519.PublicKeySize || !bytes.Equal(raw, want) {
			t.Fatalf("%q read as %x, %x", s, k.ID, k.Key)
		}
	})
}

// FuzzVerify: whatever the data and the signature file, a signature verifies only for what the
// key's holder signed, the fixture's data with its trusted comment, which Verify returns as signed.
func FuzzVerify(f *testing.F) {
	k, err := upgrade.ParsePublicKey(fixturePub)
	if err != nil {
		f.Fatal(err)
	}
	for _, sig := range []string{fixtureSig, fixtureLegacy, strings.ReplaceAll(fixtureSig, "\n", "\r\n"), fixtureSig + "\n\n", "nope\n"} {
		f.Add([]byte(fixtureData), []byte(sig))
	}
	f.Fuzz(func(t *testing.T, data, sig []byte) {
		trusted, err := k.Verify(data, sig)
		if err == nil && (string(data) != fixtureData || trusted != "devvm 0.2.0") {
			t.Fatalf("verified %q with the trusted comment %q", data, trusted)
		}
	})
}
