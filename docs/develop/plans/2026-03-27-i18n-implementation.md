# i18n 多语言支持实现计划

> 版本：v1.0
> 日期：2026-03-27
> 设计文档：[2026-03-27-i18n-design.md](./2026-03-27-i18n-design.md)

## 实施阶段

### Phase 1: 前端 i18n 基础设施

#### Task 1.1: 安装依赖

```bash
cd frontend && npm install vue-i18n@9
```

**验证**：`package.json` 包含 `vue-i18n`

#### Task 1.2: 创建 locales 目录结构

创建以下文件：

```
frontend/src/locales/
├── index.js           # i18n 配置
├── zh-CN.json         # 中文翻译
└── en.json            # 英文翻译
```

**locales/index.js**：
```javascript
import { createI18n } from 'vue-i18n'
import zhCN from './zh-CN.json'
import en from './en.json'

const savedLocale = localStorage.getItem('locale') || 'zh-CN'

export const i18n = createI18n({
  legacy: false,
  locale: savedLocale,
  fallbackLocale: 'en',
  messages: {
    'zh-CN': zhCN,
    en
  }
})

export function setLocale(locale) {
  i18n.global.locale.value = locale
  localStorage.setItem('locale', locale)
  document.documentElement.lang = locale
}
```

#### Task 1.3: 修改 main.js

```javascript
// 添加导入
import { createI18n } from 'vue-i18n'
import zhCnLocale from 'element-plus/dist/locale/zh-cn.mjs'
import enLocale from 'element-plus/dist/locale/en.mjs'
import { i18n } from './locales'

// Element Plus locale
const elementLocale = computed(() =>
  i18n.global.locale.value === 'zh-CN' ? zhCnLocale : enLocale
)

// 注册 i18n
app.use(i18n)

// Element Plus 配置 locale
// 注意：需要在组件中动态切换
```

### Phase 2: 字符串提取与替换

#### Task 2.1: 创建翻译文件

**zh-CN.json** 结构：

```json
{
  "app": {
    "title": "Binlog Server 运维控制台",
    "description": "优先发现异常、失败与延迟任务，并在详情中完成处置。"
  },
  "nav": {
    "overview": "总览",
    "tasks": "任务列表",
    "sources": "源库覆盖",
    "workers": "Worker 运维",
    "alerts": "异常与告警"
  },
  "metrics": {
    "abnormal": "异常任务",
    "failed": "失败任务",
    "delayed": "延迟任务",
    "running": "运行中任务",
    "total": "总任务",
    "normal": "正常任务"
  },
  "btn": {
    "createTask": "新建任务",
    "batchCreate": "批量创建",
    "refresh": "刷新",
    "settings": "设置",
    "alertOnly": "仅看告警",
    "resetFilter": "重置筛选",
    "query": "查询",
    "clear": "清空",
    "cancel": "取消",
    "save": "保存",
    "edit": "编辑",
    "start": "启动",
    "stop": "停止",
    "delete": "删除",
    "detail": "详情",
    "previewValidate": "预览校验",
    "clearPreview": "清空预览",
    "startBatchCreate": "开始批量创建",
    "retryUpload": "重试上传失败文件",
    "collapseMenu": "折叠菜单",
    "expandMenu": "展开菜单"
  },
  "form": {
    "taskName": "任务名",
    "clusterKey": "Cluster Key",
    "host": "主机",
    "port": "端口",
    "user": "用户",
    "password": "密码",
    "serverId": "Server ID",
    "flavor": "Flavor",
    "semiSync": "SemiSync",
    "retentionDays": "保留天数",
    "startMode": "起点模式",
    "file": "文件",
    "position": "位置",
    "gtidSet": "GTID",
    "replUser": "复制用户",
    "replPassword": "复制密码",
    "serverIdStart": "起始 ServerID",
    "autoStart": "创建后启动",
    "passwordHint": "编辑时留空=不修改"
  },
  "table": {
    "id": "ID",
    "name": "名称",
    "taskState": "任务状态",
    "ownerWorker": "归属 Worker",
    "leaseRisk": "Lease 风险",
    "replicationStatus": "复制状态",
    "delay": "延迟(s)",
    "source": "源库",
    "lastEventTime": "最近事件时间",
    "semiSync": "SemiSync",
    "actions": "操作",
    "lineNo": "行号",
    "validation": "校验"
  },
  "state": {
    "CREATED": "已创建",
    "STARTING": "启动中",
    "RUNNING": "运行中",
    "RETRY_BACKOFF": "重试退避",
    "STOPPING": "停止中",
    "STOPPED": "已停止",
    "FAILED": "失败"
  },
  "replication": {
    "NORMAL": "正常",
    "DELAYED": "延迟",
    "ABNORMAL": "异常",
    "IDLE": "空闲",
    "noProgress": "未收到复制事件",
    "delayExceedsThreshold": "延迟超过阈值",
    "runnerError": "复制错误",
    "taskStateError": "任务状态异常",
    "abnormalNoDetail": "复制异常（暂无详细错误）"
  },
  "msg": {
    "settingsSaved": "设置已保存",
    "taskCreated": "任务已创建",
    "taskUpdated": "任务已更新",
    "taskStarted": "任务 #{id} 已启动",
    "taskStopped": "任务 #{id} 已停止",
    "taskDeleted": "任务 #{id} 已删除",
    "retryUploadTriggered": "失败文件已触发重试上传",
    "previewFirst": "请先点击"预览校验"",
    "previewFailed": "预览未通过，请修正后重试",
    "userRequired": "复制用户不能为空",
    "hostPortRequired": "请输入 host 和 port 再查询",
    "confirmDelete": "确认删除任务 #{id} ?",
    "deleteConfirmTitle": "删除确认",
    "batchCreateSuccess": "批量创建成功，共 {count} 个任务",
    "batchPartialSuccess": "成功 {success} 个，失败 {failed} 个",
    "previewComplete": "预览完成：有效 {valid}，错误 {errors}",
    "previewPassed": "预览通过：共 {count} 条",
    "gotIt": "知道了",
    "batchCreateFailedDetail": "批量创建失败明细"
  },
  "validation": {
    "clusterKeyEmpty": "cluster_key 不能为空",
    "clusterKeyInvalid": "cluster_key 不合法（仅允许字母数字._-，禁止 / \\ ..）",
    "taskNameInvalid": "任务名不合法（1-255 字符）",
    "hostInvalid": "source.host 不合法",
    "portInvalid": "source.port 不合法（1-65535）",
    "userInvalid": "source.user 不合法",
    "flavorInvalid": "source.flavor 不合法",
    "serverIdInvalid": "source.server_id 不合法（0 或 1..4294967295）",
    "startModeInvalid": "start.mode 不合法",
    "filePosRequired": "FILE_POS 模式要求合法 file/pos",
    "gtidSetRequired": "GTID 模式要求 gtid_set",
    "retentionInvalid": "storage.retention_days 不合法（1-3650）"
  },
  "empty": {
    "noTasks": "暂无任务",
    "noWorkers": "暂无 worker 数据",
    "noSources": "暂无 source coverage 数据",
    "noRunHistory": "暂无运行历史",
    "none": "暂无"
  },
  "filter": {
    "title": "运维筛选",
    "hint": "优先排查异常",
    "allTaskStates": "全部任务状态",
    "allReplicationStatuses": "全部复制状态",
    "sortByDelay": "风险优先",
    "sortByUpdated": "最近更新优先",
    "sortByName": "任务名 A-Z",
    "alertFirst": "告警任务优先显示",
    "showingCount": "当前显示 {count} 个任务"
  },
  "batch": {
    "title": "批量创建任务",
    "formatHint": "每行一条，支持三种格式：",
    "previewStatus": "预览状态：",
    "submittable": "可提交",
    "hasErrors": "有错误",
    "notPreviewed": "未预览",
    "validErrorCount": "有效 {valid} / 错误 {errors}",
    "passed": "通过",
    "failed": "失败",
    "noData": "没有可解析的数据行"
  },
  "detail": {
    "center": "任务处理中心",
    "basicInfo": "基础信息",
    "replicationAndPosition": "复制与位点",
    "leaseAndWorker": "Lease 与 Worker",
    "filesAndUpload": "文件与上传",
    "runHistory": "运行历史（最近 {limit} 条）",
    "eventTimeline": "事件时间线",
    "currentState": "当前状态",
    "replicationStatus": "复制状态",
    "delay": "延迟",
    "leaseStatus": "Lease 状态",
    "checkpoint": "当前 Checkpoint",
    "errorReason": "异常原因",
    "alertThreshold": "告警阈值",
    "lastPosition": "最近位点",
    "statusCode": "状态码",
    "lastEventTime": "最近事件时间",
    "ownerWorker": "归属 Worker",
    "epoch": "Epoch",
    "updatedAt": "更新时间",
    "uploadState": "上传状态",
    "objectKey": "对象键",
    "on": "开启",
    "off": "关闭",
    "noCheckpoint": "暂无"
  },
  "source": {
    "lookupTitle": "源库反查",
    "lookupHint": "辅助定位",
    "delayThreshold": "延迟阈值",
    "taskExists": "已存在采集任务",
    "taskNotFound": "未找到采集任务",
    "matchCount": "匹配数量：{count}"
  },
  "cluster": {
    "title": "集群视图",
    "taskCount": "任务总数",
    "runningTaskCount": "运行中任务",
    "leasedTaskCount": "持有 Lease",
    "workerCount": "{count} 个 Worker",
    "workerOnline": "在线",
    "workerOffline": "离线",
    "lastHeartbeat": "最近心跳：{time}",
    "workerTasks": "任务 {tasks} / 运行 {running} / Lease {leased}",
    "noWorkers": "暂无 worker 数据",
    "overviewNote": "Worker 明细已迁移到「Worker 运维」工作区，当前仅保留集群摘要。"
  },
  "auth": {
    "reauthRequired": "需要重新认证",
    "tokenExpired": "接口认证已失效或尚未配置。",
    "tokenPlaceholder": "输入 Bearer Token",
    "tokenHint": "配置 API 认证令牌。启用认证后需要设置此项才能访问 API。"
  },
  "placeholder": {
    "searchTask": "按任务 ID/名称搜索",
    "filterSource": "按源库 host:port 过滤",
    "hostExample": "host，例如 127.0.0.1"
  },
  "panel": {
    "sourceCoverage": "源库覆盖",
    "hosts": "{count} hosts"
  }
}
```

#### Task 2.2: 替换 App.vue 中的硬编码字符串

**Template 替换示例**：

```vue
<!-- Before -->
<h1>Binlog Server 运维控制台</h1>

<!-- After -->
<h1>{{ $t('app.title') }}</h1>
```

**Script 替换示例**：

```javascript
// Before
const TASK_STATE_LABELS = {
  CREATED: "已创建",
  RUNNING: "运行中",
  // ...
};

// After
import { useI18n } from 'vue-i18n'
const { t } = useI18n()

function stateLabel(state) {
  return t(`state.${state}`)
}
```

#### Task 2.3: 替换 api.js 中的认证消息

```javascript
// 在 api.js 中导入 i18n
import { i18n } from './locales'

// 使用翻译
const message = i18n.global.t('auth.tokenExpired')
```

### Phase 3: 语言切换 UI

#### Task 3.1: 在 Settings 对话框添加语言选择

```vue
<el-form-item :label="$t('settings.language')">
  <el-select v-model="currentLocale" @change="onLocaleChange">
    <el-option label="中文简体" value="zh-CN" />
    <el-option label="English" value="en" />
  </el-select>
</el-form-item>
```

#### Task 3.2: 实现语言切换逻辑

```javascript
import { setLocale } from './locales'

const currentLocale = ref(i18n.global.locale.value)

function onLocaleChange(locale) {
  setLocale(locale)
  // 刷新 Element Plus locale
  location.reload() // 或动态更新
}
```

### Phase 4: 文档 i18n

#### Task 4.1: 重命名现有中文文档

```bash
# 示例
mv docs/guide/README.md docs/guide/README.zh-CN.md
mv docs/guide/admin/deployment.md docs/guide/admin/deployment.zh-CN.md
# ... 其他文档
```

#### Task 4.2: 创建英文文档

基于中文文档创建对应的英文版本，保持相同的目录结构。

#### Task 4.3: 补全 release-notes-v0.1.0.md

创建缺失的 v0.1.0 英文发布说明。

## 文件清单

### 新增文件

| 文件 | 描述 |
|------|------|
| `frontend/src/locales/index.js` | i18n 配置 |
| `frontend/src/locales/zh-CN.json` | 中文翻译 |
| `frontend/src/locales/en.json` | 英文翻译 |
| `docs/guide/README.md` | 英文学习入口 |
| `docs/guide/concepts/*.md` | 英文概念文档 (6 个) |
| `docs/guide/admin/*.md` | 英文运维指南 (4 个) |
| `docs/guide/dev/*.md` | 英文开发指南 (7 个) |
| `docs/guide/reference/*.md` | 英文参考文档 (3 个) |
| `docs/security.md` (重命名) | 英文安全指南 |

### 修改文件

| 文件 | 变更 |
|------|------|
| `frontend/package.json` | 添加 vue-i18n 依赖 |
| `frontend/src/main.js` | 初始化 i18n |
| `frontend/src/App.vue` | 替换 ~175 字符串 |
| `frontend/src/api.js` | 替换认证消息 |
| `docs/guide/*.zh-CN.md` | 重命名现有中文文档 |

## 验证步骤

### 前端验证

1. 启动开发服务器：`cd frontend && npm run dev`
2. 验证默认语言为中文
3. 打开设置，切换为 English
4. 验证所有 UI 文本切换为英文
5. 刷新页面，验证语言偏好保持
6. 验证 Element Plus 组件文本（分页、空状态等）

### 文档验证

```bash
# 检查文档完整性
ls -la docs/guide/*.md docs/guide/*.zh-CN.md
```

## 预计工作量

| 阶段 | 时间 |
|------|------|
| Phase 1: 基础设施 | 30 分钟 |
| Phase 2: 字符串替换 | 2-3 小时 |
| Phase 3: 语言切换 UI | 30 分钟 |
| Phase 4: 文档翻译 | 分批进行 |
| **前端总计** | **约 4 小时** |
