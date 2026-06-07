//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func main() {
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")
	mount("devtmpfs", "/dev", "devtmpfs")

	fmt.Println("VMM_PROBE_START")
	printFile("/proc/cmdline")
	printFile("/proc/partitions")
	printDir("/sys/block")
	printDir("/dev")
	printFile("/proc/modules")
	printDmesg()
	fmt.Println("VMM_PROBE_DONE")
}

func mount(source, target, fstype string) {
	_ = os.MkdirAll(target, 0755)
	if err := syscall.Mount(source, target, fstype, 0, ""); err != nil {
		fmt.Printf("mount %s on %s failed: %v\n", fstype, target, err)
	}
}

func printFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("=== %s error ===\n%v\n", path, err)
		return
	}
	fmt.Printf("=== %s ===\n%s\n", path, data)
}

func printDir(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		fmt.Printf("=== %s error ===\n%v\n", path, err)
		return
	}
	fmt.Printf("=== %s ===\n", path)
	for _, entry := range entries {
		target := ""
		if link, err := os.Readlink(filepath.Join(path, entry.Name())); err == nil {
			target = " -> " + link
		}
		fmt.Println(entry.Name() + target)
	}
}

func printDmesg() {
	buf := make([]byte, 256*1024)
	n, err := syscall.Klogctl(3, buf)
	if err != nil {
		fmt.Printf("=== dmesg error ===\n%v\n", err)
		return
	}
	fmt.Printf("=== dmesg ===\n%s\n", buf[:n])
}
