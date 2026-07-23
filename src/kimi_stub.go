//go:build !(darwin || linux)

package main

import (
	"fmt"
	"os"
)

// The Kimi surface needs a PTY; Windows support (ConPTY) is planned.
func cmdKimi(_ []string) {
	fmt.Fprintln(os.Stderr, "adtention kimi: not supported on this platform yet")
	os.Exit(1)
}
