package cbtservice

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Canonical paths must remain beneath directories that the unprivileged web
// process cannot rename or replace, including parents above their immediate dir.
func secureAncestors(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return errors.New("privileged agent paths require ancestors without group or world write access")
		}
		if err := secureOwner(info); err != nil {
			return err
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func secureOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return errors.New("privileged agent files must belong to root or the service user")
	}
	return nil
}
