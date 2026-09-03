package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// selfSignedPEM makes a throwaway CA certificate. httptest reuses one
// built-in certificate for every TLS server, so a second distinct bundle
// has to be generated to tell transport caching apart.
func selfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// tlsServer starts an HTTPS test server with a self-signed cert and
// returns it along with that cert as a PEM bundle - the shape an
// operator's spec.caBundleSecretRef Secret holds.
func tlsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, string(caPEM)
}

func TestCABundleTrustsPrivateCA(t *testing.T) {
	srv, caPEM := tlsServer(t)

	client, err := NewAWXClient(srv.URL, APIBasePathLegacy, "token", TLSOptions{CABundlePEM: caPEM})
	if err != nil {
		t.Fatalf("NewAWXClient: %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with the server's CA trusted: %v", err)
	}
}

func TestUntrustedCARejected(t *testing.T) {
	srv, _ := tlsServer(t)

	client, err := NewAWXClient(srv.URL, APIBasePathLegacy, "token", TLSOptions{})
	if err != nil {
		t.Fatalf("NewAWXClient: %v", err)
	}
	err = client.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded against an untrusted self-signed server; verification is not happening")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate verification error, got: %v", err)
	}
}

func TestInsecureSkipVerifyStillWorks(t *testing.T) {
	srv, _ := tlsServer(t)

	client, err := NewAWXClient(srv.URL, APIBasePathLegacy, "token", TLSOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("NewAWXClient: %v", err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with verification skipped: %v", err)
	}
}

// Transports are cached by TLS configuration because building one per
// reconcile leaks an idle connection pool every pass.
func TestTransportsAreCachedPerTLSConfig(t *testing.T) {
	_, caPEM := tlsServer(t)
	otherPEM := selfSignedPEM(t)

	system1, err := transportFor(TLSOptions{})
	if err != nil {
		t.Fatalf("transportFor(system): %v", err)
	}
	system2, _ := transportFor(TLSOptions{})
	if system1 != system2 {
		t.Error("two system-root clients got different transports")
	}

	ca1, err := transportFor(TLSOptions{CABundlePEM: caPEM})
	if err != nil {
		t.Fatalf("transportFor(ca): %v", err)
	}
	ca2, _ := transportFor(TLSOptions{CABundlePEM: caPEM})
	if ca1 != ca2 {
		t.Error("the same CA bundle got two transports")
	}
	if ca1 == system1 {
		t.Error("a CA bundle reused the system-root transport")
	}

	other, _ := transportFor(TLSOptions{CABundlePEM: otherPEM})
	if other == ca1 {
		t.Error("a different CA bundle reused another bundle's transport")
	}
}

func TestCABundleWithoutCertificatesIsPermanent(t *testing.T) {
	_, err := NewAWXClient("https://awx.example.com", APIBasePathLegacy, "token", TLSOptions{
		CABundlePEM: "definitely not a certificate",
	})
	if err == nil {
		t.Fatal("expected an error for a CA bundle holding no certificates")
	}
	if !errors.Is(err, errPermanentConfig) {
		t.Fatalf("expected a permanent configuration error, got: %v", err)
	}
}
