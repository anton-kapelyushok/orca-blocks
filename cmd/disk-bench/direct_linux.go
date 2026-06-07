//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func openForRead(path string, direct bool) (*os.File, error) {
	if !direct {
		return os.Open(path)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func alignedBuffer(size int) []byte {
	raw := make([]byte, size+4096)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	offset := int((4096 - (addr % 4096)) % 4096)
	return raw[offset : offset+size]
}
