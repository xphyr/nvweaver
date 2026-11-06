package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testCert is a self-signed certificate for 127.0.0.1, so that it's also its own CA.
type testCert struct {
	certPEM, keyPEM []byte
	pool            *x509.CertPool
}

var testSerial atomic.Int64

func newTestCert(t *testing.T, notAfter time.Time) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(testSerial.Add(1)),
		Subject:               pkix.Name{CommonName: "nvweaver test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return testCert{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pool:    pool,
	}
}

// tlsFixture is an HTTPS server on loopback, using certificate files that a test can replace.
type tlsFixture struct {
	certPath, keyPath, addr string
	logs                    *bytes.Buffer
}

func (f tlsFixture) write(t *testing.T, cert testCert) {
	t.Helper()
	writeFile(t, f.certPath, cert.certPEM)
	writeFile(t, f.keyPath, cert.keyPEM)
}

// writeFile replaces a file with a rename, as a careful renewal tool does.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func newTLSFixture(t *testing.T, cert testCert) tlsFixture {
	t.Helper()
	dir := t.TempDir()
	f := tlsFixture{certPath: filepath.Join(dir, "cert.pem"), keyPath: filepath.Join(dir, "key.pem"), logs: &bytes.Buffer{}}
	f.write(t, cert)
	cfg, err := NewTLSConfig(f.certPath, f.keyPath, slog.New(slog.NewTextHandler(f.logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }),
		ErrorLog: slog.NewLogLogger(slog.New(slog.NewTextHandler(io.Discard, nil)).Handler(), slog.LevelError),
	}
	srv.TLSConfig = cfg
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	f.addr = l.Addr().String()
	return f
}

// get makes a new connection each time, as the certificate is chosen in the handshake.
func (f tlsFixture) get(pool *x509.CertPool, maxVersion uint16) (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, MaxVersion: maxVersion},
		},
	}
	resp, err := client.Get("https://" + f.addr + "/")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func (f tlsFixture) mustServe(t *testing.T, cert testCert) {
	t.Helper()
	body, err := f.get(cert.pool, 0)
	if err != nil {
		t.Fatalf("a client trusting this certificate was refused: %v", err)
	}
	if body != "hello" {
		t.Fatalf("got %q", body)
	}
}

func (f tlsFixture) mustNotServe(t *testing.T, cert testCert) {
	t.Helper()
	if _, err := f.get(cert.pool, 0); err == nil {
		t.Fatal("this certificate is still being served")
	}
}

func TestTLSIsOptionalButNotHalfSet(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := NewTLSConfig("", "", log)
	if cfg != nil || err != nil {
		t.Fatalf("with neither set: %v, %v", cfg, err)
	}
	for _, c := range [][2]string{{"/some/cert", ""}, {"", "/some/key"}} {
		_, err := NewTLSConfig(c[0], c[1], log)
		if err == nil || !strings.Contains(err.Error(), "must be set together") {
			t.Errorf("%q: got %v", c, err)
		}
	}
}

func TestTLSRefusesFilesItCantUseAtStartup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	one, other := newTestCert(t, time.Now().Add(time.Hour)), newTestCert(t, time.Now().Add(time.Hour))
	certPath, keyPath := filepath.Join(dir, "cert"), filepath.Join(dir, "key")
	if _, err := NewTLSConfig(certPath, keyPath, log); err == nil {
		t.Error("files that don't exist were accepted")
	}
	writeFile(t, certPath, one.certPEM)
	writeFile(t, keyPath, other.keyPEM)
	if _, err := NewTLSConfig(certPath, keyPath, log); err == nil {
		t.Error("a key that doesn't match its certificate was accepted")
	}
	writeFile(t, keyPath, []byte("not a key"))
	if _, err := NewTLSConfig(certPath, keyPath, log); err == nil {
		t.Error("a key that isn't one was accepted")
	}
}

func TestTLSServesOnlyModernVersionsToClientsThatTrustIt(t *testing.T) {
	cert := newTestCert(t, time.Now().Add(time.Hour))
	f := newTLSFixture(t, cert)
	f.mustServe(t, cert)
	if _, err := f.get(cert.pool, tls.VersionTLS12); err != nil {
		t.Errorf("TLS 1.2 was refused: %v", err)
	}
	if _, err := f.get(cert.pool, tls.VersionTLS11); err == nil {
		t.Error("TLS 1.1 was accepted")
	}
	f.mustNotServe(t, newTestCert(t, time.Now().Add(time.Hour)))
}

func TestTLSPicksUpARenewalWithoutRestarting(t *testing.T) {
	first, second := newTestCert(t, time.Now().Add(time.Hour)), newTestCert(t, time.Now().Add(time.Hour))
	f := newTLSFixture(t, first)
	f.mustServe(t, first)
	f.mustNotServe(t, second)
	f.write(t, second)
	f.mustServe(t, second)
	f.mustNotServe(t, first)
}

func TestTLSKeepsServingThroughABadRenewal(t *testing.T) {
	first, second := newTestCert(t, time.Now().Add(time.Hour)), newTestCert(t, time.Now().Add(time.Hour))
	f := newTLSFixture(t, first)

	// Halfway through a renewal, with the new certificate and the old key.
	writeFile(t, f.certPath, second.certPEM)
	f.mustServe(t, first)
	f.mustServe(t, first)
	if n := strings.Count(f.logs.String(), "keeping the current"); n != 1 {
		t.Errorf("logged %d times for one bad change, in:\n%s", n, f.logs)
	}

	// And then it finishes.
	writeFile(t, f.keyPath, second.keyPEM)
	f.mustServe(t, second)

	// A file that disappears doesn't stop the service either.
	if err := os.Remove(f.keyPath); err != nil {
		t.Fatal(err)
	}
	f.mustServe(t, second)
	f.mustServe(t, second)
	if n := strings.Count(f.logs.String(), "keeping the current"); n != 2 {
		t.Errorf("logged %d times for two bad changes, in:\n%s", n, f.logs)
	}

	// It recovers from that too.
	f.write(t, first)
	f.mustServe(t, first)
}

func TestTLSWarnsAboutExpiry(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"a long way off", time.Now().Add(365 * 24 * time.Hour), "certificate loaded"},
		{"soon", time.Now().Add(24 * time.Hour), "expires soon"},
		{"already", time.Now().Add(-time.Minute), "has expired"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTLSFixture(t, newTestCert(t, c.notAfter))
			if !strings.Contains(f.logs.String(), c.want) {
				t.Errorf("%q not in:\n%s", c.want, f.logs)
			}
		})
	}
}
