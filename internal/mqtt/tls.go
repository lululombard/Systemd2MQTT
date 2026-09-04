package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"

	"github.com/lululombard/Systemd2MQTT/internal/config"
)

// BuildTLSConfig turns mqtt.tls.* into a tls.Config. With every field empty the result is
// a plain TLS 1.2+ config that trusts the system roots.
func BuildTLSConfig(t config.TLSConfig) (*tls.Config, error) {
	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // opt-in from the config, warned below
	}
	if t.InsecureSkipVerify {
		slog.Warn("mqtt.tls.insecure_skip_verify is on, the broker certificate is not checked")
	}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt.tls.ca_file: no certificates found in %s", t.CAFile)
		}
		tc.RootCAs = pool
	}
	if t.CertFile != "" || t.KeyFile != "" {
		if t.CertFile == "" || t.KeyFile == "" {
			return nil, fmt.Errorf("mqtt.tls.cert_file and mqtt.tls.key_file must be set together")
		}
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("mqtt.tls client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}
