//go:build !linux

package blksnap

import (
	"os"
	"os/exec"
	"time"
)

func secureOwner(os.FileInfo) error  { return nil }
func secureAncestors(string) error   { return nil }
func configureProcess(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
func syncDir(string) error           { return nil }
