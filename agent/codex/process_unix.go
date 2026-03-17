//go:build darwin || linux

package codex

import (
	"os"
	"os/exec"
	"syscall"
)

func prepareCommandForGroupKill(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func processGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return 0
	}
	return pgid
}

func killCommandProcessGroup(cmd *exec.Cmd, pgid int) error {
	if pgid > 0 {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil || err == syscall.ESRCH {
			return nil
		} else if cmd != nil && cmd.Process != nil {
			return cmd.Process.Kill()
		} else {
			return err
		}
	}
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && err != os.ErrProcessDone {
			return err
		}
	}
	return nil
}
