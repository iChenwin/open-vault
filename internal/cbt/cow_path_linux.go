package cbt

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Private leaf permissions alone do not prevent renaming that leaf through a
// writable parent. Protect the full source-local path to the COW directory.
func protectCOWHierarchy(mount, cow string) error {
	if !pathWithin(mount, cow) {
		return errors.New("COW directory is outside the source mount")
	}
	for path := cow; ; path = filepath.Dir(path) {
		st, e := os.Lstat(path)
		if e != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return errors.New("COW ancestors must be directories without group or world write access")
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok || (owner.Uid != 0 && owner.Uid != uint32(os.Geteuid())) {
			return errors.New("COW ancestors must belong to root or the tracking user")
		}
		if path == mount {
			return nil
		}
		if filepath.Dir(path) == path {
			return errors.New("COW hierarchy did not reach the source mount")
		}
	}
}
