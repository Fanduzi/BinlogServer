// Package main is the release-prep gate: release-check [-published=auto|gh|off] vX.Y.Z [git-ref].
// input: a release tag, an optional git ref (default: the working tree) in the current repo, and the repo's published GitHub releases (gh)
// output: one line per check; exit 1 when a release surface is missing or stale, 2 on a usage error
// pos: run by `make release-check` (via scripts/check-release-prep.sh) and by the release-prep job in release.yml (-published=gh); the code and its tests are internal/releaseprep (Main)
// note: if this file changes, update this header and cmd/README.md.
package main

import (
	"os"

	"binlog_server/internal/releaseprep"
)

func main() { os.Exit(releaseprep.Main(os.Args[1:], ".", os.Stdout, os.Stderr)) }
