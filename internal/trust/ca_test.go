package trust

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
)

const day = 24 * time.Hour

// readCA reads the CA in certPath and keyPath as the proxy loads it: an ECDSA key, tls's pair.
func readCA(t *testing.T, certPath, keyPath string) *x509.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pair.PrivateKey.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("the key is a %T", pair.PrivateKey)
	}
	c, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// both reads the VM's current CA and its next one.
func both(t *testing.T, ca paths.CA) (cur, next *x509.Certificate) {
	t.Helper()
	return readCA(t, ca.Cert(), ca.Key()), readCA(t, ca.NextCert(), ca.NextKey())
}

// writeTestCA writes a CA valid from notBefore to notAfter, with a key of its own, to certPath and
// keyPath, and returns its serial.
func writeTestCA(t *testing.T, certPath, keyPath string, notBefore, notAfter time.Time) *big.Int {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "test"}, NotBefore: notBefore, NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return serial
}

// A VM's first CA lasts 30 days, from an hour ago, and its next one starts a week and an hour
// before it ends and lasts as long; both stay while the current one has over a week left. The keys
// are the user's alone, and no CA names a host.
func TestInitCA(t *testing.T) {
	ca := paths.CA{Dir: filepath.Join(t.TempDir(), "ca")}
	if err := InitCA(t.Context(), ca); err != nil {
		t.Fatal(err)
	}
	cur, next := both(t, ca)
	if life := cur.NotAfter.Sub(cur.NotBefore); life != caLife+caSkew {
		t.Errorf("CA valid for %v, want %v", life, caLife+caSkew)
	}
	if time.Since(cur.NotBefore) < 59*time.Minute || time.Until(cur.NotAfter) < caLife-time.Minute {
		t.Errorf("CA from %v to %v, want from an hour ago for 30 days", cur.NotBefore, cur.NotAfter)
	}
	if !next.NotBefore.Equal(cur.NotAfter.Add(-caRenew-caSkew)) || next.NotAfter.Sub(next.NotBefore) != caLife+caSkew {
		t.Errorf("next CA from %v to %v; the current one ends %v", next.NotBefore, next.NotAfter, cur.NotAfter)
	}
	for _, c := range []*x509.Certificate{cur, next} {
		if !c.IsCA || !c.MaxPathLenZero || len(c.DNSNames) != 0 || c.KeyUsage&x509.KeyUsageCertSign == 0 {
			t.Errorf("CA = %+v", c)
		}
	}
	if cur.SerialNumber.Cmp(next.SerialNumber) == 0 || bytes.Equal(cur.RawSubjectPublicKeyInfo, next.RawSubjectPublicKeyInfo) ||
		cur.Subject.String() == next.Subject.String() {
		t.Error("the next CA is the current one, or has its name")
	}
	for name, want := range map[string]os.FileMode{"ca.key": 0o600, "next.key": 0o600, "ca.pem": 0o644, "next.pem": 0o644, ".": 0o700} {
		if st, err := os.Stat(filepath.Join(ca.Dir, name)); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want %v", name, st.Mode().Perm(), err, want)
		}
	}
	if err := InitCA(t.Context(), ca); err != nil {
		t.Fatal(err)
	}
	if c, n := both(t, ca); c.SerialNumber.Cmp(cur.SerialNumber) != 0 || n.SerialNumber.Cmp(next.SerialNumber) != 0 {
		t.Error("a current CA, or its next one, was replaced")
	}
}

// Once the current CA has under a week left, the next one, started an hour before, takes its
// place, and a new next one follows it.
func TestInitCASwitch(t *testing.T) {
	ca := paths.CA{Dir: t.TempDir()}
	now := time.Now()
	end := now.Add(caRenew - time.Minute)
	writeTestCA(t, ca.Cert(), ca.Key(), end.Add(-caLife), end)
	serial := writeTestCA(t, ca.NextCert(), ca.NextKey(), end.Add(-caRenew-caSkew), end.Add(-caRenew+caLife))
	if err := InitCA(t.Context(), ca); err != nil {
		t.Fatal(err)
	}
	cur, next := both(t, ca)
	if cur.SerialNumber.Cmp(serial) != 0 {
		t.Errorf("current CA %v, want the next one, %v", cur.SerialNumber, serial)
	}
	if next.SerialNumber.Cmp(serial) == 0 || !next.NotBefore.Equal(cur.NotAfter.Add(-caRenew-caSkew)) {
		t.Errorf("next CA %v from %v, want a new one from a week and an hour before %v", next.SerialNumber, next.NotBefore, cur.NotAfter)
	}
}

// What can't serve as the current CA, or its next, is made again: one with under a week left and
// no next one ready, one that lasts too long, a next one that ends first or hasn't started yet
// when it's needed, one that is the current one (a switch that stopped halfway), and a key
// without its certificate.
func TestInitCAReplaced(t *testing.T) {
	now := time.Now()
	type ca struct{ from, to time.Time }
	fresh := ca{now.Add(-time.Hour), now.Add(caLife - time.Hour)}
	follows := ca{fresh.to.Add(-caRenew - caSkew), fresh.to.Add(-caRenew - caSkew + caLife)}
	for _, tc := range []struct {
		name          string
		cur, next     *ca // nil: none
		keepCur, keep bool
	}{
		{name: "none"},
		{name: "three days left, no next", cur: &ca{now.Add(-27 * day), now.Add(3 * day)}},
		{name: "three days left, the next not started", cur: &ca{now.Add(-27 * day), now.Add(3 * day)}, next: &ca{now.Add(time.Hour), now.Add(30 * day)}},
		{name: "three days left, the next with as little", cur: &ca{now.Add(-27 * day), now.Add(3 * day)}, next: &ca{now.Add(-27 * day), now.Add(3 * day)}},
		{name: "ten years", cur: &ca{now.Add(-time.Hour), now.AddDate(10, 0, 0)}},
		{name: "a next that ends first", cur: &fresh, next: &ca{now.Add(-time.Hour), fresh.to.Add(-time.Hour)}, keepCur: true},
		{name: "a next that starts too late", cur: &fresh, next: &ca{fresh.to.Add(-time.Hour), fresh.to.Add(caLife)}, keepCur: true},
		{name: "a next that lasts too long", cur: &fresh, next: &ca{follows.from, follows.from.AddDate(1, 0, 0)}, keepCur: true},
		{name: "a next that follows", cur: &fresh, next: &follows, keepCur: true, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := paths.CA{Dir: t.TempDir()}
			var cs, ns *big.Int
			if tc.cur != nil {
				cs = writeTestCA(t, dir.Cert(), dir.Key(), tc.cur.from, tc.cur.to)
			}
			if tc.next != nil {
				ns = writeTestCA(t, dir.NextCert(), dir.NextKey(), tc.next.from, tc.next.to)
			}
			if err := InitCA(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			cur, next := both(t, dir)
			if kept := cs != nil && cur.SerialNumber.Cmp(cs) == 0; kept != tc.keepCur {
				t.Errorf("current CA kept: %v, want %v", kept, tc.keepCur)
			}
			if kept := ns != nil && next.SerialNumber.Cmp(ns) == 0; kept != tc.keep {
				t.Errorf("next CA kept: %v, want %v", kept, tc.keep)
			}
			if !tc.keepCur && (time.Until(cur.NotAfter) < caLife-2*time.Hour || cur.NotAfter.Sub(cur.NotBefore) != caLife+caSkew) {
				t.Errorf("current CA from %v to %v, want a new one", cur.NotBefore, cur.NotAfter)
			}
			if !tc.keep && !next.NotBefore.Equal(cur.NotAfter.Add(-caRenew-caSkew)) {
				t.Errorf("next CA from %v; the current one ends %v", next.NotBefore, cur.NotAfter)
			}
		})
	}

	// A switch that stopped halfway, the next CA's files copied to the current one's: a new next.
	dir := paths.CA{Dir: t.TempDir()}
	writeTestCA(t, dir.Cert(), dir.Key(), fresh.from, fresh.to)
	for _, f := range [][2]string{{dir.Cert(), dir.NextCert()}, {dir.Key(), dir.NextKey()}} {
		b, err := os.ReadFile(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f[1], b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := InitCA(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if cur, next := both(t, dir); cur.SerialNumber.Cmp(next.SerialNumber) == 0 {
		t.Error("the next CA is still the current one")
	}

	// A key without its certificate is replaced, with a certificate of its own.
	if err := os.Remove(dir.Cert()); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	both(t, dir)
	// So is a certificate without its key.
	if err := os.Remove(dir.Key()); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	both(t, dir)
}

// Launches of one VM at once leave a current CA and a next one whose keys and certificates match,
// each launch's InitCA in turn (the CA folder's lock).
func TestInitCATogether(t *testing.T) {
	for round := range 20 {
		ca := paths.CA{Dir: filepath.Join(t.TempDir(), "ca")}
		start := make(chan struct{})
		errs := make(chan error, 8)
		for range 8 {
			go func() {
				<-start
				errs <- InitCA(t.Context(), ca)
			}()
		}
		close(start)
		for range 8 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range [][2]string{{ca.Cert(), ca.Key()}, {ca.NextCert(), ca.NextKey()}} {
			if _, err := tls.LoadX509KeyPair(f[0], f[1]); err != nil {
				t.Fatalf("round %d: %s: %v", round, filepath.Base(f[0]), err)
			}
		}
		if cur, next := both(t, ca); !next.NotBefore.Equal(cur.NotAfter.Add(-caRenew - caSkew)) {
			t.Fatalf("round %d: the next CA doesn't follow the current one", round)
		}
	}
}

// A current CA whose key isn't its certificate's, as a crash between the two writes would leave
// it, is made again.
func TestInitCAMismatched(t *testing.T) {
	ca := paths.CA{Dir: t.TempDir()}
	if err := InitCA(t.Context(), ca); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(ca.NextKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca.Key(), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(t.Context(), ca); err != nil {
		t.Fatal(err)
	}
	both(t, ca)
}

// InitCA waits for the CA folder's lock no longer than its ctx.
func TestInitCACancelled(t *testing.T) {
	ca := paths.CA{Dir: t.TempDir()}
	unlock, err := platform.LockFile(t.Context(), ca.Lock())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := InitCA(ctx, ca); err == nil {
		t.Fatal("InitCA went on without the lock")
	}
	if _, err := os.Stat(ca.Cert()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("InitCA wrote without the lock: %v", err)
	}
}

// A CA folder InitCA can't write fails it.
func TestInitCAFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InitCA(t.Context(), paths.CA{Dir: filepath.Join(file, "ca")}); err == nil {
		t.Error("InitCA wrote under a file")
	}
	// A certificate or a key it can't write (a folder in its place) fails it too, the current
	// one's or the next one's.
	for _, name := range []string{"ca.pem", "next.pem", "ca.key", "next.key"} {
		ca := paths.CA{Dir: t.TempDir()}
		if err := os.Mkdir(filepath.Join(ca.Dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := InitCA(t.Context(), ca); err == nil {
			t.Errorf("InitCA wrote over a folder, %s", name)
		}
	}
}

// Bundle joins the CAs into one PEM file, none for none.
func TestBundle(t *testing.T) {
	if b := (CAs{}).Bundle(); len(b) != 0 {
		t.Errorf("no CAs = %q", b)
	}
	if b := (CAs{[]byte("a\n"), []byte("b\n")}).Bundle(); string(b) != "a\nb\n" {
		t.Errorf("Bundle = %q", b)
	}
}
