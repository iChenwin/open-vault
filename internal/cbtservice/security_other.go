//go:build !linux

package cbtservice

import (
	"os"
	"os/exec"
	"time"
)

func secureAncestors(string) error   { return nil }
func secureOwner(os.FileInfo) error  { return nil }
func configureProcess(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }
