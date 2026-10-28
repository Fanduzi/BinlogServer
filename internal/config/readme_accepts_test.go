// Package config provides module-level functionality for config.
// input: README.md, README_ZH.md and the repo VERSION file, through readmeFixture
// output: a failure when a routine README edit (an h3, an install snippet, a FAQ paragraph, a tool version, ...) breaks a guard rule, or when a README's cap leaves less room than the largest routine edit (the headroom check)
// pos: the accept side of the README guard in readme_doc_test.go, and how a README's byte cap is sized
// note: if this file changes, update this header and module README.md.
package config

import (
	"fmt"
	"strings"
	"testing"
)

// readmeEdit is one routine edit, in English for README.md and in Chinese for
// README_ZH.md.
type readmeEdit struct{ en, zh string }

func (e readmeEdit) text(rel string) string {
	if rel == "README_ZH.md" {
		return e.zh
	}
	return e.en
}

// insertRoutineEdit puts text in its own paragraph before the README's
// second-to-last h2, the way checkReadmeAccepts applies a routine edit.
func insertRoutineEdit(readme, text string, c readmeContract) string {
	anchor := c.headings[len(c.headings)-2] + "\n"
	return strings.Replace(readme, anchor, text+"\n\n"+anchor, 1)
}

// readmeRoutineEdits are the routine edits QA found failing, the numbers that
// are not BinlogServer versions, and the rule boundaries.
func readmeRoutineEdits(t *testing.T, f readmeFixture) map[string]readmeEdit {
	t.Helper()
	ver := f.ver
	line := strings.Join(strings.SplitN(ver, ".", 3)[:2], ".")
	faqEN := "FAQ: if the task stays in RETRY_BACKOFF after a network change, check that the source host still resolves, that the replication user can still connect from the worker, and that the binlog the task needs is still on the source. The Console task page shows the last error, and the events list shows each retry with its time. When the binlog is gone, restore from the latest full backup and start a new task from it; the old task's segments stay readable."
	faqZH := "常见问题：网络变更后任务一直停在 RETRY_BACKOFF 时，先确认源库主机名还能解析、复制账号还能从 worker 连上源库、任务需要的 binlog 还在源库上。控制台任务页显示最后一次错误，事件列表按时间列出每次重试。binlog 已经被清掉时，从最近一次全量备份恢复，再从它新建任务；旧任务的分段仍然可读。"
	return map[string]readmeEdit{
		"an h3 without versions":                {"### More detail\n\nSee the deployment guide.", "### 常见问题\n\n见部署指南。"},
		"an install snippet":                    {"```bash\nVER=" + ver + "\ncurl -fsSLO https://github.com/Fanduzi/BinlogServer/releases/download/v" + ver + "/binlog-server_${VER}_linux_amd64.tar.gz\n```", "```bash\nVER=" + ver + "\ncurl -fsSLO https://github.com/Fanduzi/BinlogServer/releases/download/v" + ver + "/binlog-server_${VER}_linux_amd64.tar.gz\n```"},
		"an image tag":                          {"docker run mysql:8.0.36", "用 docker run mysql:8.0.36 起一个源库。"},
		"a second example address":              {"META_DSN host 10.0.0.16.", "元数据库示例地址 10.0.0.16。"},
		"a four-part address on VERSION's line": {"Bind to " + line + ".1.2 in the lab.", "实验环境绑定 " + line + ".1.2。"},
		"a tool version":                        {"promtool v2.53.0 check rules", "用 promtool v2.53.0 检查告警规则。"},
		"two-part versions":                     {"MySQL 8.0 and 8.4.", "MySQL 8.0 和 8.4。"},
		"a release link":                        {"[v" + ver + "](https://github.com/Fanduzi/BinlogServer/releases/tag/v" + ver + ") and [latest](https://github.com/Fanduzi/BinlogServer/releases/latest)", "[v" + ver + "](https://github.com/Fanduzi/BinlogServer/releases/tag/v" + ver + ") 和 [最新版本](https://github.com/Fanduzi/BinlogServer/releases/latest)"},
		"a FAQ paragraph":                       {faqEN, faqZH},
		"a checklist bullet":                    {"- **Watch the retry budget:** alert when `consecutive_source_failures` stays above 5 for ten minutes, and page before it reaches the cap.", "- **关注重试预算：** `consecutive_source_failures` 连续十分钟高于 5 时告警，在达到上限之前通知值班。"},
		"PromQL and ratios":                     {"`histogram_quantile(0.99, rate(x[5m]))`, sample ratio 0.5, a 0.5s timeout, 0.25 days.", "`histogram_quantile(0.99, rate(x[5m]))`，采样率 0.5，超时 0.5s，保留 0.25 天。"},
		"a Go tool version":                     {"go install golang.org/x/tools/cmd/deadcode@v0.52.0", "go install golang.org/x/tools/cmd/deadcode@v0.52.0"},
		"a pinned tool on VERSION's line":       {"go install example.com/tool@v" + f.other(t, 1), "go install example.com/tool@v" + f.other(t, 1)},
		"other software versions":               {"Tested against MySQL 8.4.13 and 5.7.44, Go 1.27.3, MariaDB 11.4.2.", "测试覆盖 MySQL 8.4.13 和 5.7.44、Go 1.27.3、MariaDB 11.4.2。"},
		"a month and year":                      {"MySQL 5.7 reached end of life in October 2023.", "MySQL 5.7 已于 2023 年 10 月停止支持。"},
		"VERSION itself":                        {"Also v" + ver + ".", "另见 v" + ver + "。"},
		"a heading in code fences":              {"```\n## not a heading\n# nor this\n```\n\n~~~\n## nor this\n~~~", "```\n## 不是标题\n```\n\n~~~\n## 也不是\n~~~"},
		"a rule after a blank line":             {"Text.\n\n---", "文字。\n\n---"},
		"a rule after a code block":             {"Run:\n```\nmake\n```\n---", "运行：\n```\nmake\n```\n---"},
	}
}

// largestRoutineEdit returns the routine edit that adds the most bytes to the
// README rel, and how many.
func largestRoutineEdit(edits map[string]readmeEdit, rel string) (string, int) {
	name, most := "", 0
	for n, e := range edits {
		if size := len(e.text(rel)) + len("\n\n"); size > most || size == most && n < name {
			name, most = n, size
		}
	}
	return name, most
}

// readmeHeadroomProblem is the headroom check: the README under its cap still
// has room for the largest routine edit, which checkReadmeAccepts applies on
// top of it. It fails before the README itself reaches its cap, so it names
// the headroom rather than the routine edit that no longer fits.
func readmeHeadroomProblem(body string, c readmeContract, edits map[string]readmeEdit) string {
	name, need := largestRoutineEdit(edits, c.rel)
	if room := c.maxBytes - len(body); room < need {
		return fmt.Sprintf("headroom check: %s is %d bytes, %d under its %d-byte cap, but the routine-edit tests need %d (the largest is %q); "+
			"the README has used the %d bytes of growth its cap allows. If the growth is legitimate, raise maxBytes for %s in readmeContracts to %d (the size plus %d plus %d)",
			c.rel, len(body), room, c.maxBytes, need, name, readmeGrowth, c.rel, len(body)+readmeGrowth+need, readmeGrowth, need)
	}
	return ""
}

// TestReadmeHeadroom checks that each real README leaves room for the largest
// routine edit, and logs the cap it would get today.
func TestReadmeHeadroom(t *testing.T) {
	f := realReadmeFixture(t)
	edits := readmeRoutineEdits(t, f)
	for _, c := range readmeContracts {
		if p := readmeHeadroomProblem(f.files[c.rel], c, edits); p != "" {
			t.Error(p)
		}
		_, need := largestRoutineEdit(edits, c.rel)
		t.Logf("%s: %d bytes, cap %d, a fresh cap would be %d", c.rel, len(f.files[c.rel]), c.maxBytes, len(f.files[c.rel])+readmeGrowth+need)
	}
}

// TestReadmeGrowthFitsTheCap sets each cap the documented way and grows the
// README by exactly readmeGrowth bytes: it must pass the guard, the headroom
// check and every routine edit on top. One more byte fails the headroom check
// by name, while the README itself is still under its cap.
func TestReadmeGrowthFitsTheCap(t *testing.T) {
	f := realReadmeFixture(t)
	edits := readmeRoutineEdits(t, f)
	for _, c := range readmeContracts {
		readme := f.files[c.rel]
		_, need := largestRoutineEdit(edits, c.rel)
		fresh := c
		fresh.maxBytes = len(readme) + readmeGrowth + need
		grow := func(n int) string {
			return insertRoutineEdit(readme, strings.Repeat("x", n-len("\n\n")), c)
		}
		grown := grow(readmeGrowth)
		if len(grown) != len(readme)+readmeGrowth {
			t.Fatalf("%s: grew by %d bytes, want %d", c.rel, len(grown)-len(readme), readmeGrowth)
		}
		if got := readmeProblems(grown, f.ver, fresh); len(got) != 0 {
			t.Errorf("%s grown by %d bytes: %v", c.rel, readmeGrowth, got)
		}
		if p := readmeHeadroomProblem(grown, fresh, edits); p != "" {
			t.Errorf("%s grown by %d bytes: %s", c.rel, readmeGrowth, p)
		}
		for name, e := range edits {
			if got := readmeProblems(insertRoutineEdit(grown, e.text(c.rel), c), f.ver, fresh); len(got) != 0 {
				t.Errorf("%s grown by %d bytes, then %s: %v", c.rel, readmeGrowth, name, got)
			}
		}
		over := grow(readmeGrowth + 1)
		if got := readmeProblems(over, f.ver, fresh); len(got) != 0 {
			t.Errorf("%s grown by %d bytes is still under its cap: %v", c.rel, readmeGrowth+1, got)
		}
		if p := readmeHeadroomProblem(over, fresh, edits); !strings.HasPrefix(p, "headroom check: "+c.rel+" is ") || !strings.Contains(p, "raise maxBytes for "+c.rel) {
			t.Errorf("%s grown by %d bytes: headroom check %q, want it named with the cap to set", c.rel, readmeGrowth+1, p)
		}
	}
}

// checkReadmeAccepts applies each routine edit on its own, so the cases do not
// spend each other's headroom. The headroom check runs first: when the README
// has grown into the room the edits need, it says so once, and the edits run
// against a cap with just that room so their own failures still mean a rule.
func checkReadmeAccepts(t *testing.T, f readmeFixture) {
	t.Helper()
	ver := f.ver
	edits := readmeRoutineEdits(t, f)
	for _, c := range readmeContracts {
		readme := f.files[c.rel]
		roomy := c
		if p := readmeHeadroomProblem(readme, c, edits); p != "" {
			t.Error(p)
			_, need := largestRoutineEdit(edits, c.rel)
			roomy.maxBytes = len(readme) + need
		}
		for name, e := range edits {
			if got := readmeProblems(insertRoutineEdit(readme, e.text(c.rel), c), ver, roomy); len(got) != 0 {
				t.Errorf("%s: %s: %v", c.rel, name, got)
			}
		}
		pad := insertRoutineEdit(readme, strings.Repeat("x", c.maxBytes-len(readme)-2), c)
		if len(pad) != c.maxBytes {
			t.Fatalf("%s: padding built %d bytes, want %d", c.rel, len(pad), c.maxBytes)
		}
		if got := readmeProblems(pad, ver, c); len(got) != 0 {
			t.Errorf("%s: README at exactly the cap: %v", c.rel, got)
		}
		trailing := strings.Replace(readme, c.headings[1]+"\n", c.headings[1]+" \t\n", 1)
		if got := readmeProblems(trailing, ver, c); len(got) != 0 {
			t.Errorf("%s: trailing whitespace on an allowed heading: %v", c.rel, got)
		}
		// The summary bound counts characters, not bytes; checked on the
		// template alone so it does not spend the byte cap.
		var pinLine string
		for _, l := range strings.Split(readme, "\n") {
			if strings.HasPrefix(l, c.pinPrefix) {
				pinLine = l
			}
		}
		longest := c.pinPrefix + ver + c.pinSuffix + strings.Repeat("升", 200)
		if got := upgradingTemplateProblems(strings.Replace(readme, pinLine, longest, 1), ver, c); len(got) != 0 {
			t.Errorf("%s: summary at exactly 200 characters: %v", c.rel, got)
		}
	}
}
