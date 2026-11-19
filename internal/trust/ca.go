package trust

import (
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
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// Each VM has a CA of its own, whatever its tier, which its proxy
// intercepts with and which only that VM trusts, so a key that leaks
// reaches one VM. A CA lasts caLife, so a leaked key stops working within
// weeks, and has a successor, the next CA, which every launch installs in
// the VM beside it. The next CA starts caRenew and caSkew before the
// current one ends; the proxy signs with it from caSkew after its start
// (the hour lets a VM whose clock is behind see it started), and the
// launch after that point makes it the current one and makes another
// next. A VM that isn't opened again so keeps its proxy's certificates
// verifying until the next CA ends, at least caLife less caSkew after its
// last launch.
const (
	caLife  = 30 * 24 * time.Hour
	caRenew = 7 * 24 * time.Hour
	caSkew  = time.Hour
)

// InitCA readies the VM's CA in ca (paths.VM.CA) at each launch: its
// current CA, ca.pem and ca.key, and its next one, next.pem and next.key.
// A current CA with over caRenew left stays. One with less, or none, is
// replaced by the next CA when that has started and has over caRenew
// left, and by a new one valid from now otherwise. A next CA that doesn't
// follow the current one (ending after it, starting by the time it's
// replaced) is made again. Certificates are mode 644 and keys, ECDSA ("EC
// PRIVATE KEY"), mode 600, each written whole, the key first. A CA valid
// for longer than caLife is replaced too. No CA names a host. It holds the
// folder's lock (paths.CA.Lock), waiting for another launch's until ctx
// ends, so two launches at once never leave a key and a certificate of
// different runs.
func InitCA(ctx context.Context, ca paths.CA) error {
	if err := state.PrivateDir(ca.Dir); err != nil {
		return err
	}
	unlock, err := platform.LockFile(ctx, ca.Lock(), msg.Waiting(msg.StatusOf(ctx), msg.CAWaiting()))
	if err != nil {
		return err
	}
	defer unlock()
	return initCA(ca)
}

// initCA is InitCA, holding the lock.
func initCA(ca paths.CA) error {
	now := time.Now()
	cur, err := readPair(ca.Cert(), ca.Key())
	if err != nil || !cur.current(now) {
		next, nerr := readPair(ca.NextCert(), ca.NextKey())
		if nerr == nil && next.current(now) && !next.cert.NotBefore.After(now) {
			err = next.write(ca.Cert(), ca.Key())
		} else {
			next, err = makeAt(ca.Cert(), ca.Key(), now.Add(-caSkew))
		}
		if err != nil {
			return err
		}
		cur = next
	}
	if next, err := readPair(ca.NextCert(), ca.NextKey()); err == nil && next.follows(cur) {
		return nil
	}
	_, err = makeAt(ca.NextCert(), ca.NextKey(), cur.cert.NotAfter.Add(-caRenew-caSkew))
	return err
}

// makeAt makes a CA valid from notBefore (makePair) and writes it to
// certPath and keyPath.
func makeAt(certPath, keyPath string, notBefore time.Time) (pair, error) {
	p, err := makePair(notBefore)
	if err != nil {
		return p, err
	}
	return p, p.write(certPath, keyPath)
}

// pair is a CA as its files hold it: its certificate, parsed, and both
// files' PEM.
type pair struct {
	cert    *x509.Certificate
	certPEM []byte
	keyPEM  []byte
}

// readPair reads the CA in certPath and keyPath: a certificate and its
// key, which must match.
func readPair(certPath, keyPath string) (pair, error) {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // the VM's CA folder (paths.VM.CA)
	if err != nil {
		return pair{}, err
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // the VM's CA folder (paths.VM.CA)
	if err != nil {
		return pair{}, err
	}
	return pairOf(certPEM, keyPEM)
}

// pairOf is the CA in certPEM and keyPEM, which must match.
func pairOf(certPEM, keyPEM []byte) (pair, error) {
	tp, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return pair{}, err
	}
	return pair{cert: tp.Leaf, certPEM: certPEM, keyPEM: keyPEM}, nil
}

// current: the CA has over caRenew left at now, and a life of at most
// caLife (and caSkew, which a new one starts early by).
func (p pair) current(now time.Time) bool {
	c := p.cert
	return c.NotAfter.Sub(now) > caRenew && c.NotAfter.Sub(c.NotBefore) <= caLife+caSkew
}

// follows: the CA can be cur's next one, ending after it and starting by
// the time cur is replaced, with a life of at most caLife.
func (p pair) follows(cur pair) bool {
	c := p.cert
	return c.NotAfter.After(cur.cert.NotAfter) && !c.NotBefore.After(cur.cert.NotAfter.Add(-caRenew)) &&
		c.NotAfter.Sub(c.NotBefore) <= caLife+caSkew
}

// makePair makes a CA valid from notBefore for caLife and caSkew, with a
// new ECDSA P-256 key.
func makePair(notBefore time.Time) (pair, error) {
	// crypto/rand never fails: a system whose randomness does crashes Go.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	// A name of its own, its serial's, so a VM's two CAs never share a subject, which some
	// verifiers look issuers up by.
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "devvm proxy CA " + serial.Text(16), Organization: []string{"devvm"}},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(caLife + caSkew),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	kder, kerr := x509.MarshalECPrivateKey(key)
	if err := errors.Join(err, kerr); err != nil {
		return pair{}, err
	}
	return pairOf(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

// write writes the CA to certPath and keyPath, each whole. The key goes
// first: a crash between the two leaves a pair that doesn't match, which
// the next InitCA replaces.
func (p pair) write(certPath, keyPath string) error {
	if err := state.WriteFileMode(keyPath, p.keyPEM, 0o600); err != nil {
		return err
	}
	return state.WriteFileMode(certPath, p.certPEM, 0o644)
}
