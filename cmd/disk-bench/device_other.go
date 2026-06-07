//go:build !linux

package main

import "os"

func ensureDevice(path string) error {
	_, err := os.Stat(path)
	return err
}

func configureReadAhead(path string, readAheadKB int) error {
	return nil
}
