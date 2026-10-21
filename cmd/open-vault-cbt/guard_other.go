//go:build !linux

package main

import "errors"

func validateDiskSeparation(config, string) error {
	return errors.New("CBT backup requires Linux; restore and verification are portable")
}
