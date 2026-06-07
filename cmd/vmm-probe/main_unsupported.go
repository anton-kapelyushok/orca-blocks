//go:build !linux

package main

import "fmt"

func main() {
	fmt.Println("vmm-probe is only supported on Linux")
}
