//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureDetachedWorker(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func configureCommandProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateCommandProcess(pid int) error { return terminateProcessGroup(pid) }
func terminateWorker(pid int) error         { return terminateProcessGroup(pid) }

func terminateProcessGroup(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pid, 0)
		if err == syscall.ESRCH {
			return nil
		}
		if err != nil && err != syscall.EPERM {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}

	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func workerIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
