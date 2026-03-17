//go:build !darwin && !linux

package codex

import "os/exec"

func prepareCommandForGroupKill(cmd *exec.Cmd) {}

func processGroupID(cmd *exec.Cmd) int { return 0 }

func killCommandProcessGroup(cmd *exec.Cmd, _ int) error {
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
