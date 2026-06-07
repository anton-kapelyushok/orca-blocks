//go:build !linux

package main

import "os"

func openForRead(path string, direct bool) (*os.File, error) {
	return os.Open(path)
}

func alignedBuffer(size int) []byte {
	return make([]byte, size)
}
