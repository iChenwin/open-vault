//go:build !linux

package cbt

import "errors"

func externalCOWFilesystem(string, string) error { return errors.New("CBT requires Linux") }
