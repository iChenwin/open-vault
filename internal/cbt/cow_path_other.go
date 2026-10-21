//go:build !linux

package cbt

import "errors"

func protectCOWHierarchy(string, string) error { return errors.New("COW tracking requires Linux") }
