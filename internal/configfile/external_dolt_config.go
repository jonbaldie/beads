package configfile

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

const (
	ExternalDoltConfigDefaultUser = "root"
	ExternalDoltPasswordEnvVar    = "BEADS_PROXIED_SERVER_EXTERNAL_PASSWORD" // #nosec G101 -- env var name, not a credential
)

type ExternalDoltConfig struct {
	Host            string        `json:"host,omitempty"`
	Port            int           `json:"port,omitempty"`
	Socket          string        `json:"socket,omitempty"`
	User            string        `json:"user,omitempty"`
	TLSRequired     bool          `json:"tls_required,omitempty"`
	TLSCACert       string        `json:"tls_ca_cert,omitempty"`
	TLSCert         string        `json:"tls_cert,omitempty"`
	TLSKey          string        `json:"tls_key,omitempty"`
	TLSServerName   string        `json:"tls_server_name,omitempty"`
	TLSSkipVerify   bool          `json:"tls_skip_verify,omitempty"`
	KeepAlivePeriod time.Duration `json:"keep_alive_period,omitempty"`
}

func (c ExternalDoltConfig) Validate() error {
	if err := validateExternalTransport(c); err != nil {
		return err
	}
	if err := validateExternalTLSFiles(c); err != nil {
		return err
	}
	if err := validateExternalTLSRequirements(c); err != nil {
		return err
	}
	if c.KeepAlivePeriod < 0 {
		return fmt.Errorf("ExternalDoltConfig: KeepAlivePeriod %s is negative", c.KeepAlivePeriod)
	}
	return nil
}

func validateExternalTransport(c ExternalDoltConfig) error {
	hasHost := c.Host != ""
	hasPort := c.Port != 0
	hasSocket := c.Socket != ""

	if err := validateExternalTransportShape(hasSocket, hasHost, hasPort); err != nil {
		return err
	}

	if hasHost && (c.Port < 1 || c.Port > 65535) {
		return fmt.Errorf("ExternalDoltConfig: Port %d out of range [1, 65535]", c.Port)
	}

	if hasSocket && !filepath.IsAbs(c.Socket) {
		return fmt.Errorf("ExternalDoltConfig: Socket %q is not absolute", c.Socket)
	}
	return nil
}

func validateExternalTransportShape(hasSocket, hasHost, hasPort bool) error {
	if hasSocket {
		if hasHost || hasPort {
			return errors.New("ExternalDoltConfig: set either Socket OR (Host, Port), not both")
		}
		return nil
	}
	if !hasHost && !hasPort {
		return errors.New("ExternalDoltConfig: must set Socket or (Host, Port)")
	}
	if hasHost && !hasPort {
		return errors.New("ExternalDoltConfig: Host requires Port")
	}
	if hasHost {
		return nil
	}
	return errors.New("ExternalDoltConfig: Port requires Host")
}

func validateExternalTLSFiles(c ExternalDoltConfig) error {
	if err := validateExternalTLSKeyPair(c); err != nil {
		return err
	}
	return validateExternalTLSPaths(c)
}

func validateExternalTLSKeyPair(c ExternalDoltConfig) error {
	switch {
	case c.TLSCert != "" && c.TLSKey == "":
		return errors.New("ExternalDoltConfig: TLSCert set without TLSKey")
	case c.TLSCert == "" && c.TLSKey != "":
		return errors.New("ExternalDoltConfig: TLSKey set without TLSCert")
	}
	return nil
}

func validateExternalTLSPaths(c ExternalDoltConfig) error {
	if c.TLSCert != "" && !filepath.IsAbs(c.TLSCert) {
		return fmt.Errorf("ExternalDoltConfig: TLSCert %q is not absolute", c.TLSCert)
	}
	if c.TLSKey != "" && !filepath.IsAbs(c.TLSKey) {
		return fmt.Errorf("ExternalDoltConfig: TLSKey %q is not absolute", c.TLSKey)
	}
	if c.TLSCACert != "" && !filepath.IsAbs(c.TLSCACert) {
		return fmt.Errorf("ExternalDoltConfig: TLSCACert %q is not absolute", c.TLSCACert)
	}
	return nil
}

func validateExternalTLSRequirements(c ExternalDoltConfig) error {
	if !c.TLSRequired {
		return validateExternalTLSDisabled(c)
	}
	return validateExternalTLSSocket(c)
}

func validateExternalTLSDisabled(c ExternalDoltConfig) error {
	switch {
	case c.TLSCACert != "":
		return errors.New("ExternalDoltConfig: TLSCACert set without TLSRequired")
	case c.TLSCert != "" || c.TLSKey != "":
		return errors.New("ExternalDoltConfig: TLSCert/TLSKey set without TLSRequired")
	case c.TLSServerName != "":
		return errors.New("ExternalDoltConfig: TLSServerName set without TLSRequired")
	case c.TLSSkipVerify:
		return errors.New("ExternalDoltConfig: TLSSkipVerify set without TLSRequired")
	default:
		return nil
	}
}

func validateExternalTLSSocket(c ExternalDoltConfig) error {
	if c.Socket != "" && c.TLSServerName == "" && !c.TLSSkipVerify {
		return errors.New("ExternalDoltConfig: TLSRequired over Socket needs TLSServerName or TLSSkipVerify")
	}
	return nil
}
