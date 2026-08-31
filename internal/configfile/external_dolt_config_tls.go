package configfile

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

func (c ExternalDoltConfig) TLSClientConfig() (*tls.Config, error) {
	if !c.TLSRequired {
		return nil, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if c.TLSSkipVerify {
		cfg.InsecureSkipVerify = true //nolint:gosec // G402: opt-in insecure transport via the TLSSkipVerify testing flag
	} else {
		name := c.TLSServerName
		if name == "" {
			name = c.Host
		}
		cfg.ServerName = name
	}

	if c.TLSCACert != "" {
		pem, err := os.ReadFile(c.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("ExternalDoltConfig: read TLSCACert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ExternalDoltConfig: TLSCACert %q: no certificates parsed", c.TLSCACert)
		}
		cfg.RootCAs = pool
	}

	if c.TLSCert != "" {
		crt, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("ExternalDoltConfig: load client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{crt}
	}

	return cfg, nil
}
