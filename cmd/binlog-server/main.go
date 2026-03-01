// Package main provides module-level functionality for main.
// input: process args and root cobra command wiring
// output: binary entry that executes the server CLI command tree
// pos: top-level process bootstrap for binlog-server runtime
// note: if this file changes, update this header and module README.md.
package main

import (
	"log"

	"binlog_server/cmd/binlog-server/cmd"
)

// @title Binlog Server API
// @version 0.1.0
// @description Centralized MySQL binlog backup service API.
// @BasePath /
func main() {
	if err := cmd.NewRootCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}
