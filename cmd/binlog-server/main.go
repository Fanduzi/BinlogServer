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
