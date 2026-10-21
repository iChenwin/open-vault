package blksnap

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

func trustedBinary(path string) (string, error) {
	real, e := filepath.EvalSymlinks(path)
	if e != nil {
		return "", errors.New("blksnap executable must exist")
	}
	st, e := os.Lstat(real)
	if e != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && (st.Mode().Perm()&0022 != 0 || st.Mode().Perm()&0111 == 0)) {
		return "", errors.New("blksnap executable must be trusted and executable")
	}
	if e = secureOwner(st); e != nil {
		return "", e
	}
	if e = secureAncestors(filepath.Dir(real)); e != nil {
		return "", e
	}
	return real, nil
}
func privateDir(path string) (string, error) {
	real, e := filepath.EvalSymlinks(path)
	if e != nil {
		return "", errors.New("provider state directory must exist")
	}
	st, e := os.Lstat(real)
	if e != nil || !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return "", errors.New("provider state directory must be private")
	}
	if e = secureOwner(st); e != nil {
		return "", e
	}
	if e = secureAncestors(real); e != nil {
		return "", e
	}
	return real, nil
}
