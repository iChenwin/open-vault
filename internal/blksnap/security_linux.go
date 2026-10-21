package blksnap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func secureOwner(st os.FileInfo) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || (s.Uid != 0 && s.Uid != uint32(os.Geteuid())) {
		return errors.New("privileged provider path must have a trusted owner")
	}
	return nil
}
func secureAncestors(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return errors.New("provider ancestors must be trusted non-writable directories")
		}
		if e = secureOwner(st); e != nil {
			return e
		}
		if filepath.Dir(p) == p {
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
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(e, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return e
	}
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return errors.New("cannot open journal directory")
	}
	defer f.Close()
	if e = f.Sync(); e != nil {
		return errors.New("cannot sync journal directory")
	}
	return nil
}
