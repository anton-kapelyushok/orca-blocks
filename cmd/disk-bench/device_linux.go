//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func ensureDevice(path string) error {
	if _, err := os.Stat(path); err == nil || !strings.HasPrefix(path, "/dev/") {
		return err
	}
	if err := os.MkdirAll("/dev", 0755); err != nil {
		return err
	}
	if err := os.MkdirAll("/proc", 0755); err != nil {
		return err
	}
	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	name := strings.TrimPrefix(path, "/dev/")
	major, minor, err := findBlockDevice(name)
	if err != nil {
		major, minor = 254, 0
	}
	return syscall.Mknod(path, syscall.S_IFBLK|0600, int(makedev(uint32(major), uint32(minor))))
}

func configureReadAhead(path string, readAheadKB int) error {
	if readAheadKB < 0 || !strings.HasPrefix(path, "/dev/") {
		return nil
	}
	if err := os.MkdirAll("/sys", 0755); err != nil {
		return err
	}
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	name := strings.TrimPrefix(path, "/dev/")
	readAheadPath := fmt.Sprintf("/sys/block/%s/queue/read_ahead_kb", name)
	return os.WriteFile(readAheadPath, []byte(strconv.Itoa(readAheadKB)+"\n"), 0644)
}

func findBlockDevice(name string) (int, int, error) {
	raw, err := os.ReadFile("/proc/partitions")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[3] != name {
			continue
		}
		major, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, 0, err
		}
		minor, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, 0, err
		}
		return major, minor, nil
	}
	return 0, 0, fmt.Errorf("%s not found in /proc/partitions", name)
}

func makedev(major, minor uint32) uint64 {
	return (uint64(major&0x00000fff) << 8) |
		uint64(minor&0x000000ff) |
		(uint64(minor&0xffffff00) << 12) |
		(uint64(major&0xfffff000) << 32)
}
