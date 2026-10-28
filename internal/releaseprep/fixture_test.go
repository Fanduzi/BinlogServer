// Package releaseprep holds a complete fake release for the gate's own tests.
// input: a version X.Y.Z and the previous version
// output: file bodies for every release surface, each passing Check
// pos: shared fixture for the releaseprep tests (gate, CLI and bump); test-only
// note: if this file changes, update this header and README.md.
package releaseprep

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture returns a complete release-prep tree for ver (X.Y.Z) that names prev
// only where releaseprep.History allows it, the way the real tree does: the
// CHANGELOG, the README upgrade notes and release-notes links, and the
// landing timeline entries.
func Fixture(ver, prev string) map[string]string {
	r := strings.NewReplacer("@V@", ver, "@P@", prev)
	return map[string]string{
		notesEN("v" + ver): r.Replace("# Binlog Server v@V@\n\nRelease date: 2026-01-01\n\n" +
			"v@V@ keeps every sealed segment verified before upload and fixes a retention marker after an online migrate up.\n"),
		notesZH("v" + ver): r.Replace("# Binlog Server v@V@ 中文发布说明\n\n> 对应版本：`v@V@`\n\n" +
			"本版本在上传前校验每个封存段，并修复在线迁移之后保留策略标记缺失的问题。升级不需要迁移，也没有新的配置项，可以直接回滚到上一个版本。\n"),
		changelogPath: r.Replace("# Changelog\n\n## [Unreleased]\n\n## [v@V@] - 2026-01-01\n\n### Fixed\n\n- retention marker\n\n" +
			"## [v@P@] - 2025-12-01\n\n- older\n"),
		"README.md":                      r.Replace(readmeTemplate),
		"README_ZH.md":                   r.Replace(readmeZHTemplate),
		deployPath:                       r.Replace("# Deployment\n\n```bash\nVER=@V@\n```\n"),
		"config.example.yaml":            r.Replace("# BinlogServer v@V@ Comprehensive Configuration Reference\nlisten_addr: 127.0.0.1:8080\n"),
		"config.production.example.yaml": r.Replace("# Production deployment template for BinlogServer v@V@.\nlisten_addr: 0.0.0.0:8080\n"),
		versionPath:                      ver + "\n",
		// The hero, install and CTA markup is the shape the landing-page
		// cleanup keeps: the timeline and its "(Current)" marker are gone.
		landingPath: r.Replace(`<!doctype html>
<html lang="en" data-version="@V@">
<head><meta name="description" content="BinlogServer"><style>.hidden { display: none }</style></head>
<body>
<span class="tag tag-new" data-i18n="heroBadge">v@V@ — latest release</span>
<code data-i18n="step1Code">VER=@V@ OS=linux ARCH=amd64
curl -fsSL -O "https://github.com/x/releases/download/v${VER}/binlog-server_${VER}_${OS}_${ARCH}.tar.gz"</code>
<a href="#deployment" class="btn-primary" data-i18n="ctaPrimary">Get Started with v@V@</a>
<p>MySQL 8.4 LTS, MariaDB 10.3+, 127.0.0.1:8080</p>
<script>const zh = { heroBadge: 'v@V@ — 最新发布版本', ctaPrimary: '开始使用 v@V@' };</script>
</body></html>
`),
	}
}

const readmeTemplate = "# BinlogServer\n\n## Quick Start\n\n### 1. Download, verify, and unpack v@V@\n\n```bash\nVER=@V@\n" +
	"# fetch the archive\ncurl -fsSL -O \"https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_linux_amd64.tar.gz\"\n```\n\n" +
	"Published `v@V@` `checksums.txt`:\n\n```text\n" +
	"24c9d9cc572607bfb9fb21c24fac546e4748723010fa35c733777c79adce8c8d  binlog-server_@V@_linux_amd64.tar.gz\n```\n\n" +
	"```text\nbinlog-server_@V@_linux_amd64/\n  binlog-server\n```\n\n### 2. Run it\n\nStart it.\n\n" +
	"## Upgrade Notes\n\n- **v@V@: retention marker, no migration:** in-place upgrade from v@P@ is supported.\n" +
	"- **v@P@: health metrics, no migration:** optional keys.\n\n" +
	"Full release notes: [v@V@](docs/releases/release-notes-v@V@.md) · v@P@: [notes](docs/releases/release-notes-v@P@.md)\n"

const readmeZHTemplate = "# BinlogServer\n\n## 快速开始\n\n### 1. 下载、校验并解压 v@V@\n\n```bash\nVER=@V@\n" +
	"# 下载归档\ncurl -fsSL -O \"https://github.com/Fanduzi/BinlogServer/releases/download/v${VER}/binlog-server_${VER}_linux_amd64.tar.gz\"\n```\n\n" +
	"已发布的 `v@V@` `checksums.txt`：\n\n```text\n" +
	"24c9d9cc572607bfb9fb21c24fac546e4748723010fa35c733777c79adce8c8d  binlog-server_@V@_linux_amd64.tar.gz\n```\n\n" +
	"### 2. 启动\n\n启动。\n\n## 升级须知\n\n- **v@V@：保留策略标记，没有迁移：** 支持从 v@P@ 原地升级。\n" +
	"- **v@P@：健康指标，没有迁移：** 可选配置。\n\n" +
	"详细版本记录：[v@V@](docs/releases/v@V@.zh-CN.md) · v@P@：[说明](docs/releases/v@P@.zh-CN.md)\n"

// NewFixtureRepoDir builds a git repo whose v<prev> tag carries the prev release
// and whose HEAD (branch main) carries the complete ver release.
func NewFixtureRepoDir(t testing.TB, ver, prev string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(files map[string]string) {
		for path, body := range files {
			full := filepath.Join(dir, path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitRun("init", "-q", "-b", "main")
	write(Fixture(prev, "0.0.1"))
	gitRun("add", "-A")
	gitRun("commit", "-qm", "v"+prev)
	gitRun("tag", "v"+prev)
	write(Fixture(ver, prev))
	gitRun("add", "-A")
	gitRun("commit", "-qm", "v"+ver)
	return dir
}
