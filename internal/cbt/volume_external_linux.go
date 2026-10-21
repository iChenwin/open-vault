package cbt

import (
	"errors"
	"os"
	"syscall"
)

func externalCOWFilesystem(device, cow string) error {
	source, err := os.Stat(device)
	if err != nil {
		return err
	}
	destination, err := os.Stat(cow)
	if err != nil {
		return err
	}
	s, ok := source.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine source device identity")
	}
	d, ok := destination.Sys().(*syscall.Stat_t)
	if !ok || uint64(s.Rdev) == uint64(d.Dev) {
		return errors.New("external COW must be on a different filesystem from the source device")
	}
	return nil
}
