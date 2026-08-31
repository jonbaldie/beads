package dolt

import (
	"os"
	"os/exec"
	"strings"
)

// applyToCmd sets DOLT_REMOTE_USER/PASSWORD on the subprocess environment,
// isolating credentials to this specific exec.Cmd. This avoids setting
// process-wide env vars that could leak to concurrent goroutines.
func (c *remoteCredentials) applyToCmd(cmd *exec.Cmd) {
	if c.empty() {
		return
	}
	// Start with current process env, filtering out any existing credential vars
	// to prevent stale values from leaking to the subprocess.
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DOLT_REMOTE_USER=") && !strings.HasPrefix(e, "DOLT_REMOTE_PASSWORD=") {
			env = append(env, e)
		}
	}
	if c.username != "" {
		env = append(env, "DOLT_REMOTE_USER="+c.username)
	}
	if c.password != "" {
		env = append(env, "DOLT_REMOTE_PASSWORD="+c.password)
	}
	cmd.Env = env
}
