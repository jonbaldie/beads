package pidfile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jonbaldie/beads/internal/atomicfile"
)

type PidFile struct {
	Pid         int    `json:"pid"`
	Port        int    `json:"port"`
	UpstreamID  string `json:"upstream_id,omitempty"`
	Schema      int    `json:"schema,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Birth       string `json:"birth,omitempty"`
	RootID      string `json:"root_id,omitempty"`
	ControlPort int    `json:"control_port,omitempty"`
}

const SchemaV2 = 2

const (
	KindProxy       = "db-proxy"
	KindDoltBackend = "dolt-backend"
)

var (
	ErrLegacySchema = errors.New("pidfile: legacy schema")
	ErrBadPid       = errors.New("pidfile: invalid pid")
	ErrBadPort      = errors.New("pidfile: invalid port")
	ErrKindMismatch = errors.New("pidfile: kind mismatch")
	ErrMissingBirth = errors.New("pidfile: missing birth token")
)

// ValidateV2 validates the fields required for a schema v2 pidfile.
func (p *PidFile) ValidateV2(wantKind string) error {
	checks := []func() error{
		func() error { return validateSchema(p.Schema) },
		func() error { return validatePID(p.Pid) },
		func() error { return validatePorts(p.Port, p.ControlPort) },
		func() error { return validateKind(p.Kind, wantKind) },
		func() error { return validateBirth(p.Birth) },
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func validateSchema(schema int) error {
	if schema < SchemaV2 {
		return ErrLegacySchema
	}
	return nil
}

func validatePID(pid int) error {
	if pid <= 0 {
		return ErrBadPid
	}
	return nil
}

func validatePorts(port, controlPort int) error {
	if port < 1 || port > 65535 || (controlPort != 0 && (controlPort < 1 || controlPort > 65535)) {
		return ErrBadPort
	}
	return nil
}

func validateKind(kind, wantKind string) error {
	if kind != wantKind {
		return ErrKindMismatch
	}
	return nil
}

func validateBirth(birth string) error {
	if birth == "" {
		return ErrMissingBirth
	}
	return nil
}

func Path(rootDir, name string) string {
	return filepath.Join(rootDir, name)
}

func Read(rootDir, name string) (*PidFile, error) {
	data, err := os.ReadFile(Path(rootDir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pf PidFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	return &pf, nil
}

func Write(rootDir, name string, pf PidFile) error {
	data, err := json.Marshal(pf)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(Path(rootDir, name), data, 0o644)
}

func Remove(rootDir, name string) error {
	err := os.Remove(Path(rootDir, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
