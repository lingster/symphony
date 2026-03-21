// Package main provides the Symphony orchestrator CLI entrypoint.
package main

import (
	"os"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
