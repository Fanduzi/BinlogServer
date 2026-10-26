#!/usr/bin/env bash
# input: dist/ from a goreleaser run (metadata.json plus the linux_amd64 tar.gz); GITHUB_SHA,
#        and GITHUB_REF_TYPE / GITHUB_REF_NAME when run for a tag
# output: runs check-linux-release-archive.sh on that archive with EXPECT_COMMIT=GITHUB_SHA and
#         EXPECT_VERSION=the version goreleaser built (for a tag, it must also be the tag without "v")
# pos: release.yml (before and after publish) and e2e.yml release-archive (snapshot on every PR)
# note: if this file changes, update this header and module README.md.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

: "${GITHUB_SHA:?GITHUB_SHA must be set}"
version="$(jq -er '.version' dist/metadata.json)"
if [[ "${GITHUB_REF_TYPE:-}" == "tag" && "$version" != "${GITHUB_REF_NAME#v}" ]]; then
  echo "[release-identity] error: goreleaser built version ${version}, tag is ${GITHUB_REF_NAME}" >&2
  exit 1
fi
archive="dist/binlog-server_${version}_linux_amd64.tar.gz"
echo "[release-identity] expect Version: ${version}, Commit: ${GITHUB_SHA}"
EXPECT_VERSION="$version" EXPECT_COMMIT="$GITHUB_SHA" ./scripts/check-linux-release-archive.sh "$archive"
