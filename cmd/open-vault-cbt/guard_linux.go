package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func existingAncestor(path string) (os.FileInfo, error) {
	for {
		st, e := os.Stat(path)
		if e == nil {
			return st, nil
		}
		if !os.IsNotExist(e) {
			return nil, e
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil, e
		}
		path = parent
	}
}

// A different path or bind mount on the same source filesystem is not a safe
// destination: writing the repository or oracle there would feed its own COW.
func validateDiskSeparation(c config, oracle string) error {
	source, e := os.Stat(c.Volume.MountPoint)
	if e != nil {
		return errors.New("source mount is unavailable")
	}
	src, ok := source.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine source filesystem")
	}
	paths := []string{c.Repository, c.StateDir, c.KeyFile}
	if oracle != "" {
		if !filepath.IsAbs(oracle) {
			return errors.New("oracle target must be absolute")
		}
		paths = append(paths, oracle)
	}
	for _, path := range paths {
		st, e := existingAncestor(path)
		if e != nil {
			return e
		}
		dst, ok := st.Sys().(*syscall.Stat_t)
		if !ok || dst.Dev == src.Dev {
			return errors.New("repository, state, key and laboratory oracle must reside outside the source filesystem")
		}
	}
	return nil
}
