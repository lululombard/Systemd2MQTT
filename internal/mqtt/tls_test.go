package mqtt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// writeSelfSigned writes a self-signed CA cert and its key into dir and returns both paths.
func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "systemd2mqtt test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "ca.pem")
	keyPath = filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestBuildTLSConfigEmpty(t *testing.T) {
	tc, err := BuildTLSConfig(config.TLSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", tc.MinVersion)
	}
	if tc.RootCAs != nil {
		t.Error("RootCAs should be nil so the system roots are used")
	}
	if len(tc.Certificates) != 0 {
		t.Error("no client certificate expected")
	}
	if tc.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should default to false")
	}
}

func TestBuildTLSConfigCAAndClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir)

	tc, err := BuildTLSConfig(config.TLSConfig{CAFile: certPath})
	if err != nil {
		t.Fatal(err)
	}
	if tc.RootCAs == nil {
		t.Fatal("RootCAs not set from ca_file")
	}
	if len(tc.Certificates) != 0 {
		t.Error("no client certificate expected with only ca_file")
	}

	// The self-signed pair doubles as a client certificate for this test.
	tc, err = BuildTLSConfig(config.TLSConfig{CAFile: certPath, CertFile: certPath, KeyFile: keyPath, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Certificates) != 1 {
		t.Fatalf("got %d client certificates, want 1", len(tc.Certificates))
	}
	if !tc.InsecureSkipVerify {
		t.Error("InsecureSkipVerify not carried over")
	}
}

func TestBuildTLSConfigErrors(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir)
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("this is not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]config.TLSConfig{
		"missing ca":   {CAFile: filepath.Join(dir, "nope.pem")},
		"bad ca pem":   {CAFile: bad},
		"cert no key":  {CertFile: certPath},
		"key no cert":  {KeyFile: keyPath},
		"bad key pair": {CertFile: certPath, KeyFile: bad},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildTLSConfig(tc); err == nil {
				t.Errorf("BuildTLSConfig(%+v) returned no error", tc)
			}
		})
	}
}
