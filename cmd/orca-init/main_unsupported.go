//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("orca-init is only built for Linux guest rootfs images")
}
