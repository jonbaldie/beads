//go:build unix

package hooks

import (
	"os"
	"os/exec"
	"testing"
)

func TestKillHookGroupWithoutProcessIsNoOp(t *testing.T) {
	if err := killHookGroup(&exec.Cmd{}); err != nil {
		t.Fatalf("killHookGroup without a process = %v, want nil", err)
	}
}

func TestKillHookGroupAlreadyExitedIsNoOp(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 1 << 30}}
	if err := killHookGroup(cmd); err != nil {
		t.Fatalf("killHookGroup for exited process = %v, want nil", err)
	}
}
