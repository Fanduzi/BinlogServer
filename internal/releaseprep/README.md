# releaseprep

发版准备门禁（`make release-check`、`release.yml` 的 `release-prep` job），以及每个 PR 都跑的同一套版本检查（`internal/config` 的 `TestVersionPinsMatchVERSIONFile`，e2e.yml 的 `release-prep-selftest` 也跑 `make release-check`）。tag 打在发版准备提交上，这个提交必须带齐下面所有内容，检查读的是 tag 指向的那个提交（不带 ref 时读工作区）。

门禁分两半：
- **反向检查（主门禁）**：找出所有已跟踪文件里上一个版本的每一种写法，除了 `History` 白名单允许的地方，一律失败，并给出 `文件:行`。漏改的地方不需要事先登记，不管旧版本藏在注释、属性、脚本还是被 CSS 隐藏的元素里都拦得住。
- **正向检查**：清单 `Surfaces` 里的每个发布面必须写上本版本。

## 防什么，不防什么
这道门禁防的是**发版时忘了改某处**，不防贡献者**故意**把文字藏起来。用 CSS 或属性把文字视觉上藏起来的办法没有尽头（`opacity`、`display`、`inert`、`content-visibility`、`@media`、iframe、外部样式表、JS……），每加一种就是一段要维护、容易漏测的代码，所以 r4 把这些启发式全部删掉了。留下的只有便宜、稳的归一化：HTML 注释、`<template>`、`<script>`、`<style>` 的内容和标签属性（包括 `<meta content>`）不算正向检查要的发布位置。真正的防线是反向检查：旧版本不管怎么藏，只要还在文件里就失败。故意视觉隐藏不在范围内，交给代码评审。

## Files
- `text.go`：把文件原始字节变成检查读的文本。任何文件都不会因为编码被跳过：带 BOM 的 UTF-16（LE/BE）解码，UTF-8 BOM 去掉，NUL 字节删掉（没有 BOM 的 UTF-16 和二进制里的 ASCII 字符串照样能读），非法 UTF-8 换成 U+FFFD。markup 文件（`.html`、`.htm`、`.md`）的显示文本 `shownText`：去注释，去 `<template>`/`<script>`/`<style>` 的内容（没闭合的到文件尾），去标签（块级标签换行），解实体。
- `history.go`：反向检查 `checkStale` 和发版历史白名单 `History`。`findVersion` 认的写法：带不带 `v`/`V`；`.`、`_`、`-` 分隔（归档名、目录名）；前导零（`01.02.03`、`1.2.003`）；版本中间夹的行内标签和注释（`v1.<b>2</b>.3`、`1.2<!-- -->.3`）；`%2E`/`%2D`/`%5F`；`&#46;` 之类的 HTML 实体；零宽字符；`．`、`。`、`․` 等点号；`‐`、`–`、`—`、`－` 等横线；全角、阿拉伯-印度等任何 Unicode 十进制数字。更长的数字不算（`10.1.2.3`、`1.2.30`、`1.2.3.1` 都不是 `1.2.3`，同一个分隔符接着还是数字也不算，按位分组的 `1_000_000_000` 里不藏版本）；逗号分隔（`1,2,3`）和点号两边带空格（`1 . 2 . 3`）故意不认，它们更常是别的数字。注释不豁免：注释里的旧版本也是没改的旧版本。符号链接不跟随：跳过并在通过信息里写明数量和路径（链接目标本身是已跟踪文件时照常扫描）。
- `manifest.go`：发布面清单 `Surfaces`（唯一的一份）和它的规则。
- `releaseprep.go`：`Check(src, rel, "vX.Y.Z")`：`rel` 是 tag 门禁看到的 tag 和已发布 release，`nil` 就是每个 PR 对 `VERSION` 跑的检查。两种都走 `checkTree`：中英文发布说明、CHANGELOG 小节、反向检查、清单，所以 PR 改了 `VERSION` 却没带发布说明或 CHANGELOG 也会失败。
- `source.go`：`Dir`（工作区，`git ls-files`，`Lstat` 识别链接）、`GitRef`（某个提交，`git ls-tree` 按 mode 120000 识别链接，加一次 `git cat-file --batch`）、`MergedTags`（`git tag --merged`）、`ResolveRef`。
- `cli.go`：`release-check` 命令行（`Main`），`cmd/release-check` 只调它，所以门禁和它的命令行测试都在这个包里。退出码 0/1/2 是 tag 门禁唯一的结果：`script_test.go` 复制真实仓库，经由 `release.yml` 调的 `scripts/check-release-prep.sh` 构建并运行真实二进制，完整 bump 退出 0、漏改一处退出 1、被跟踪文件换成同名目录（读不了，root 也一样）退出 2、工具本身编译不过退出 2，每次跑完脚本的临时目录都必须删掉。`TestReleasePrepStepOnATag` 把 `release.yml` 里 release-prep 那一步的 `run` 锁成固定文本（`env` 只有 `GH_TOKEN`），再像 runner 一样用 `bash -e` 在打了 tag 的仓库副本里执行它（假 `gh` 报当前版本已发布）：tag 漏改一处退出 1，完整的 tag 退出 0，branch 运行直接跳过。`internal/lintguard` 的 `TestReleaseGatesCannotBeBypassed` 保证 release 和它依赖的 job（release-prep、ci 以及 ci 调用的 e2e.yml 的每个 job）及其 step 上没有 `continue-on-error`，`if` 只能是 `allowedIfs` 表里的条件。
- `fixture_test.go`：完整的假版本（`Fixture`）和带上一版 tag 的假仓库（`NewFixtureRepoDir`），只给测试用。
- `releaseprep_test.go`、`history_test.go`、`followup_test.go`、`cli_test.go`（命令行）、`bump_test.go`（真实仓库 bump）：每条规则都有反例，假版本对（`releaseprep_test.go` 的 `versions`）有三位数的 patch 和跨 minor 的一组，抓字符串排序、前缀匹配和未转义的点。

## 上一个版本和要扫的旧版本
`Releases{Merged, Published}`：`Merged` 是发布提交历史里的 tag（`git tag --merged <ref>`），`Published` 是已发布（非草稿、非预发布）GitHub release 的 tag（`gh release list --exclude-drafts --exclude-pre-releases`）。

要扫哪些旧版本只有一个来源：`scanVersions`。tag 门禁和每个 PR 的 go test 调的都是 `Check`，只是后者不传 tag。
- 上一个版本 `P` = 比本版本旧的最新一个**已发布** release：有 `Published` 时取 `Published ∩ Merged`；不知道哪些已发布时（`-published=off`、`auto` 下 gh 不可用、go test）取 `CHANGELOG.md` 的 `## [vX.Y.Z]` 小节（注释和代码块里的不算；知道历史时还要求 tag 在历史里）。
- 另外再扫：`CHANGELOG.md` 里有、或者历史里打了 tag，比 `P` 新、比本版本旧的每个版本。发版失败留下的版本（tag 打了、release 没发出来）已经把它的版本号写进了所有发布面，下一次发版漏改的地方就停在它上面。例：`v0.(y).(z+1)` 打了 tag 没发布，发 `v0.(y).(z+2)` 时部署指南还停在 `z+1`：`-published=gh` 下 `P` 是 `z`、另外扫 `z+1`，离线和 go test 下 `P` 就是 `z+1`，三种情况都失败（`cli_test.go` 的 `TestUnpublishedTaggedReleaseIsScanned`）。
- 跨 minor/major 也一样：`VERSION` 升到下一个 minor（`0.y.0`）时，上一个版本是最后发布的 `0.(y-1).x`，留在归档路径里的旧版本照样失败（`TestMinorBumpScansThePreviousMinor`，测试的版本对里也有一组跨 minor 的）。`History` 里没有 README 的规则，README 里的旧版本就靠这条扫描。
- 历史里不能有比本版本新的 tag。历史里一个 tag 都没有时（浅克隆，`fetch-depth: 1`）明确报错，提示 `git fetch --tags`；`release.yml` 和 selftest 都用 `fetch-depth: 0`，`internal/lintguard` 的 `TestReleasePrepGateIsWired` 锁住这一点。
- `release.yml` 用 `-published=gh`（gh 失败即 rc=2）；本地默认 `auto`，gh 不可用时打一行 `note` 后用 CHANGELOG。

## 真实仓库能直接发下一个版本
`bump_test.go` 的 `TestRealTreeBumpsCleanly` 把真实的已跟踪文件复制出来，只按流程改发布面清单里的文件、中英文发布说明和 CHANGELOG，分别升到下一个 patch、minor、major，然后在这份副本上跑门禁和 `go test ./internal/config ./internal/releaseprep`，必须全部通过（`-short` 时跳过）。`TestRealTreeBumpMissingAnySiteFails` 每次少改一处（任一发布面、任一份发布说明或 CHANGELOG），门禁和 PR 检查都必须失败。所以某份文档在清单以外写了当前版本号，写进去的那个 PR 就会失败，而不是等到发版。releaseprep 自己的文档和非测试代码里的例子一律不写真实版本号。

## 检查项（版本 `X.Y.Z`，上一个版本 `P`，比较一律精确相等）
1. tag 只能是 `vX.Y.Z`。`vX.Y.Z-rc1` 这类预发布 tag 故意拒绝（rc=2）：每个 tag 都是文档指向的正式版本。
2. 上一个版本按上一节确定。
3. `docs/releases/release-notes-vX.Y.Z.md`：第一行非空行是 `# Binlog Server vX.Y.Z`，标题下正文至少 80 个字符（不算标题行和 HTML 注释）。
4. `docs/releases/vX.Y.Z.zh-CN.md`：第一行是只写这个版本的 `# ` 标题，正文至少 80 个字符且至少 20 个汉字。
5. `CHANGELOG.md`：有 `## [vX.Y.Z]`（或 `## vX.Y.Z`）标题，下一个 `## ` 之前有非空内容；注释和代码块（` ``` ` 和 `~~~`）里的标题不算。
6. 反向检查：除 `History` 允许的地方以外，没有一处写着 `P`（任何写法）。每个 PR 和 tag 都跑。
7. 清单 `Surfaces`（显示文本见 `text.go`；可见字符串以版本结尾时，后面不能接着还是版本，比如 `v1.2.30`、`v1.2.3.1`、`v1.2.3-rc1` 都不是 `v1.2.3`）：
   - `README.md`、`README_ZH.md`：显示文本里有 `` Latest release: `vX.Y.Z` `` / `` 最新版本：`vX.Y.Z` ``；每个 `VER=`（带引号、带 `v`、多一段、带 `-rc`、空值、`{VER}` 之类不是 `{ver}` 的占位符都算）都等于 `X.Y.Z`，至少有一个可见；安装小节（第一个 h2–h4 标题含 `unpack` 或 `解压`，到下一个同级或更高级标题为止，代码块里的 `#` 行不算标题）标题写这个版本，小节里的所有版本都是这个版本。
   - `docs/guide/admin/deployment.md`：`VER=` 同上。
   - `docs/guide/admin/upgrade.md`、`upgrade.zh-CN.md`（`NewestNote: GuideNote`）：最新的一条逐版本说明是本版本，说明从新到旧。
   - `config.example.yaml`、`config.production.example.yaml`：有 `BinlogServer vX.Y.Z`，文件里其他版本也只能是这个。
   - `docs/landing/index.html`：`<html data-version>` 恰好是 `X.Y.Z`（第一个 `<html>` 标签），显示文本里有 `vX.Y.Z — latest release`、`Get Started with vX.Y.Z` 和时间线的 `vX.Y.Z (Current)`，至少一个可见的 `VER=X.Y.Z`。
   - `VERSION`：恰好一行 `X.Y.Z`。

`Surface` 的规则：`Visible`、`Attrs`、`VERPins`、`InstallSection`、`OnlyThisVersion`、`Exact`、`NewestNote`、`ScriptUsesPlaceholder`、`Mentions`。版本规则只写在 `Surfaces` 和 `History` 里，不要在别的测试里再写一份。

## 白名单 `History`
每条 `HistoryRule` 必须写 `Why`；作用范围三选一：
- 不写 `Line` 和 `Section`：整个文件；
- `Section`（标题正则）：这个小节的每一行，到下一个同级或更高级标题为止（认代码块，块里的 `#` 不算标题）；
- `Line`（正则）：行里每一处 `P` 都必须落在 `Line` 的某个匹配里，同一行别处再有一份照样失败；行匹配 `Unless` 时规则不生效。`Line` 和 `Section` 同时写是错误。`{v}`、`{p}` 代表本版本和上一个版本。

当前白名单（r4 收窄：去掉了 `**/*.go`、`**/testdata/**`、`**/package.json`、UI 打包产物、`docs/develop/**`，这些地方今天都没有发版历史，也没有理由有）：
- 整个文件：`CHANGELOG.md`、`docs/releases/**`、`docs/adr/**`、`docs/design/**`（带日期的决策和设计记录，写的是当时发布的版本，不按版本改）、`**/*_test.go`（测试写假版本和某个行为从哪个版本开始，不发布）、`migrations/**`（迁移写加它的版本，发布后不再改）、`go.mod`、`go.sum`、`**/package-lock.json`（依赖版本）。
- 小节：`docs/guide/admin/upgrade.md`、`upgrade.zh-CN.md` 的 `## Per-version notes` / `## 逐版本说明`（每个版本一条说明，小节里的说明文字也可以写旧版本）。
- 行：e2e 原地升级那一条腿故意从上一个版本开始：`e2e.yml` 的 `old_ref: vP` 和 `in-place upgrade from vP`，`upgrade-legacy-failback.sh` 的 `plain from vP`，`docs/production-readiness.md` 的 `` `plain` 从 vP ``，`scripts/e2e/README.md` 的 `` `plain` 从 `vP` `` 和 `` 从 `vP` 直接升级 ``。
- 行（#565 合入后删）：落地页时间线的旧条目（`<div class="timeline-version">vX.Y.Z</div>`、`release2`–`release9` 的 `<p>` 和 JS 字符串），以及写着本版本的 `release1`。每条只覆盖那一个元素。

新出现的、确实是历史的旧版本写进 `History`，写明原因；失败信息会提示这一点。releaseprep 自己的非测试代码里的例子一律用 `1.2.3` 这类假版本，否则下一次发版会扫到它们。

## #565 rebase 时的改动（已在合并模拟里跑通：`go test ./internal/config ./internal/releaseprep` 和对真实仓库当前 `VERSION` 跑的 `release-check` 都通过）
- #565（`internal/config` 两处冲突）：
  - `manifest.go`：落地页一项换成 `Attrs: {{"html","data-version"}}`、`Visible: {"v{v} — latest release", "Get Started with v{v}"}`、`VERPins`、`OnlyThisVersion`、`ScriptUsesPlaceholder`、`Mentions: 4`（去掉 `v{v} (Current)`）。
  - `history.go`：删掉落地页的三条规则。
  - `fixture_test.go`：`Fixture` 的落地页换成 `followup_test.go` 里原来 `fixture565` 的页面（hero、安装、CTA 的标记本来就一样，只去掉时间线，`<script>` 改用 `{ver}`）。
  - `followup_test.go`：`fixture565()` 改成 `return Fixture(ver, prev)`；`manifest565()` 幂等，`TestManifest565` 不用改。
  - `releaseprep_test.go`：删掉 `timeline Current names the previous version` 一条用例（时间线没了）。
  - 冲突：`internal/config/config_doc_test.go` 保留 #564 这边（`releaseprep.Check(…, nil, …)`；#565 删掉的三行在 #564 里已经不存在）；`internal/config/README.md` 两边的行都保留（#564 的 `config_doc_test.go` 行和 #565 新增的 `landing_doc_test.go` 行）。

## 已知取舍
- pin 在 tag 提交里就改成新版本，release 资产在 tag 之后几分钟（`ci` 跑完、`release` 上传）才出现，这段时间 README 和落地页的下载链接会 404。这是选定的流程：一个发版准备 PR，快进合入，tag 打在这个提交上，tag 之后不再提 pin PR。
- 归档的 SHA-256 只有 `release` 构建完才知道，所以门禁只查 README checksum 行里的归档名，不查哈希值。
- 故意视觉隐藏不在范围内（见上）：正向检查可能被 CSS 隐藏的元素"满足"，但旧版本残留仍然被反向检查拦住。
