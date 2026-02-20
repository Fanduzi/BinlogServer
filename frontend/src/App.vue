<template>
  <div class="page-shell">
    <div class="orb orb-a" />
    <div class="orb orb-b" />

    <header class="hero">
      <div class="hero-copy">
        <p class="kicker"><i class="fa-solid fa-wave-square" /> BINLOG SERVER</p>
        <h1>Binlog Server</h1>
        <p class="hero-desc">
          Lag, health, and source coverage in one place.
        </p>
      </div>
      <div class="hero-actions">
        <el-button type="primary" @click="openCreate">
          <i class="fa-solid fa-plus" /> 新建任务
        </el-button>
        <el-button @click="openBatchCreate">
          <i class="fa-solid fa-layer-group" /> 批量创建
        </el-button>
        <el-button :loading="loading" @click="refreshAll">
          <i class="fa-solid fa-rotate" /> 刷新
        </el-button>
      </div>
    </header>

    <section class="metric-grid">
      <article class="metric-card">
        <p><i class="fa-solid fa-layer-group" /> 总任务</p>
        <strong>{{ dashboard.summary.total }}</strong>
      </article>
      <article class="metric-card">
        <p><i class="fa-solid fa-play" /> 运行中</p>
        <strong>{{ dashboard.summary.running }}</strong>
      </article>
      <article class="metric-card">
        <p><i class="fa-solid fa-circle-check" /> 正常</p>
        <strong>{{ dashboard.summary.normal }}</strong>
      </article>
      <article class="metric-card">
        <p><i class="fa-solid fa-hourglass-half" /> 延迟</p>
        <strong>{{ dashboard.summary.delayed }}</strong>
      </article>
      <article class="metric-card">
        <p><i class="fa-solid fa-triangle-exclamation" /> 异常</p>
        <strong>{{ dashboard.summary.abnormal }}</strong>
      </article>
      <article class="metric-card">
        <p><i class="fa-solid fa-bug" /> 失败</p>
        <strong>{{ dashboard.summary.failed }}</strong>
      </article>
    </section>

    <section class="workspace">
      <aside class="left-pane">
        <el-card shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-magnifying-glass" /> 源库反查</span>
              <span class="panel-hint">host:port</span>
            </div>
          </template>

          <div class="lookup-form">
            <el-input
              v-model="sourceQuery.host"
              placeholder="host，例如 127.0.0.1"
              clearable
            />
            <el-input-number
              v-model="sourceQuery.port"
              :min="1"
              :max="65535"
              controls-position="right"
            />
            <div class="btn-row">
              <el-button type="primary" :loading="loading" @click="applySourceFilter">查询</el-button>
              <el-button @click="clearSourceFilter">清空</el-button>
            </div>
          </div>

          <div class="meta-line">
            <span>阈值 {{ dashboard.threshold_seconds }}s</span>
            <span>{{ formatTs(dashboard.generated_at) }}</span>
          </div>

          <div v-if="lookup.checked" class="lookup-state">
            <el-tag :type="lookup.exists ? 'success' : 'info'">
              {{ lookup.exists ? "已存在拉取任务" : "未找到拉取任务" }}
            </el-tag>
            <span>匹配数量：{{ lookup.count }}</span>
          </div>
        </el-card>

        <el-card shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-sliders" /> 任务筛选</span>
              <span class="panel-hint">quick filters</span>
            </div>
          </template>

          <div class="filter-stack">
            <el-input v-model="uiFilter.keyword" clearable placeholder="按任务 ID/名称搜索" />
            <el-input v-model="uiFilter.sourceKeyword" clearable placeholder="按源库 host:port 过滤" />
            <el-select v-model="uiFilter.taskState">
              <el-option label="全部任务状态" value="ALL" />
              <el-option v-for="state in taskStates" :key="state" :label="state" :value="state" />
            </el-select>
            <el-select v-model="uiFilter.replicationStatus">
              <el-option label="全部复制状态" value="ALL" />
              <el-option v-for="status in replicationStatuses" :key="status" :label="status" :value="status" />
            </el-select>
            <el-select v-model="uiFilter.sortBy">
              <el-option label="延迟高优先" value="delay_desc" />
              <el-option label="最近更新优先" value="updated_desc" />
              <el-option label="任务名 A-Z" value="name_asc" />
            </el-select>
            <div class="switch-row">
              <span>仅看告警任务</span>
              <el-switch v-model="uiFilter.onlyAlert" />
            </div>
            <el-button @click="resetUiFilter">重置筛选</el-button>
          </div>
        </el-card>
      </aside>

      <section class="right-pane">
        <el-card shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-sitemap" /> Cluster 视图</span>
              <span class="panel-hint">{{ cluster.overview.worker_count }} workers</span>
            </div>
          </template>

          <div class="cluster-overview-grid">
            <div class="cluster-stat-cell">
              <p>任务总数</p>
              <strong>{{ cluster.overview.task_count }}</strong>
            </div>
            <div class="cluster-stat-cell">
              <p>运行中任务</p>
              <strong>{{ cluster.overview.running_task_count }}</strong>
            </div>
            <div class="cluster-stat-cell">
              <p>持有 Lease</p>
              <strong>{{ cluster.overview.leased_task_count }}</strong>
            </div>
          </div>

          <div class="cluster-worker-list">
            <div
              v-for="worker in workerRows"
              :key="worker.worker_id"
              class="cluster-worker-item"
            >
              <div class="cluster-worker-head">
                <strong>{{ worker.worker_id }}</strong>
                <el-tag size="small" :type="worker.online ? 'success' : 'info'">
                  {{ worker.online ? "在线" : "离线" }}
                </el-tag>
              </div>
              <p>
                任务 {{ worker.task_count }} / 运行 {{ worker.running }} / Lease {{ worker.leased }}
              </p>
              <p class="cluster-worker-time">
                最近心跳：{{ formatTs(worker.last_seen_at) }}
              </p>
            </div>
            <el-empty
              v-if="workerRows.length === 0"
              description="暂无 worker 数据"
              :image-size="56"
            />
          </div>
        </el-card>

        <el-card shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-network-wired" /> 源库覆盖</span>
              <span class="panel-hint">{{ dashboard.sources.length }} hosts</span>
            </div>
          </template>

          <div class="source-board">
            <div
              v-for="item in dashboard.sources"
              :key="`${item.host}:${item.port}`"
              class="source-cell"
            >
              <p class="source-name">{{ item.host }}:{{ item.port }}</p>
              <p class="source-stats">
                任务 {{ item.task_count }} / 运行 {{ item.running }} / 正常 {{ item.normal }} / 延迟 {{ item.delayed }} / 异常 {{ item.abnormal }}
              </p>
            </div>
          </div>
        </el-card>

        <el-card shadow="never" class="panel-card table-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-table" /> 任务明细</span>
              <div class="panel-title-actions">
                <span class="panel-hint">筛选后 {{ filteredTasks.length }} / 总计 {{ dashboard.tasks.length }}</span>
                <el-button size="small" @click="openBatchCreate">
                  <i class="fa-solid fa-layer-group" /> 批量创建
                </el-button>
              </div>
            </div>
          </template>

          <el-table
            :data="pagedTasks"
            border
            stripe
            row-key="task.id"
            @row-click="onRowClick"
          >
            <el-table-column label="ID" width="70">
              <template #default="{ row }">{{ row.task.id }}</template>
            </el-table-column>
            <el-table-column label="名称" min-width="180">
              <template #default="{ row }">{{ row.task.name }}</template>
            </el-table-column>
            <el-table-column label="任务状态" width="120">
              <template #default="{ row }">
                <el-tag size="small" :type="stateTagType(row.task.state)">{{ row.task.state }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="归属 Worker" min-width="130">
              <template #default="{ row }">{{ ownerWorkerLabel(row.task) }}</template>
            </el-table-column>
            <el-table-column label="Lease 风险" width="100">
              <template #default="{ row }">
                <el-tag size="small" :type="leaseRiskTagType(row.task)">{{ leaseRiskLabel(row.task) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="复制状态" width="150">
              <template #default="{ row }">
                <div class="replication-cell">
                  <el-tag size="small" :type="replicationTagType(row.replication.status)">
                    {{ row.replication.status }}
                  </el-tag>
                  <el-tooltip
                    v-if="hasReplicationReason(row.replication)"
                    :content="formatReplicationReason(row.replication)"
                    placement="top"
                    effect="light"
                  >
                    <i class="fa-solid fa-circle-info reason-tip-icon" @click.stop />
                  </el-tooltip>
                </div>
              </template>
            </el-table-column>
            <el-table-column label="延迟(s)" width="100">
              <template #default="{ row }">{{ formatDelay(row.replication.delay_seconds, row.replication.has_progress) }}</template>
            </el-table-column>
            <el-table-column label="源库" min-width="170">
              <template #default="{ row }">{{ sourceLabel(row.task) }}</template>
            </el-table-column>
            <el-table-column label="最近事件时间" min-width="170">
              <template #default="{ row }">{{ formatTs(row.replication.last_event_at) }}</template>
            </el-table-column>
            <el-table-column label="SemiSync" width="90">
              <template #default="{ row }">{{ row.task.source?.semi_sync ? "ON" : "OFF" }}</template>
            </el-table-column>
            <el-table-column label="操作" width="286" fixed="right" class-name="action-col">
              <template #default="{ row }">
                <div class="action-row">
                  <el-button class="action-btn" size="small" @click.stop="showDetail(row.task)">详情</el-button>
                  <el-button class="action-btn" size="small" @click.stop="openEdit(row.task)">编辑</el-button>
                  <el-button class="action-btn" size="small" type="success" @click.stop="onStart(row.task)">启动</el-button>
                  <el-button class="action-btn" size="small" type="warning" @click.stop="onStop(row.task)">停止</el-button>
                  <el-button class="action-btn" size="small" type="danger" @click.stop="onDelete(row.task)">删除</el-button>
                </div>
              </template>
            </el-table-column>
          </el-table>

          <div class="pager-wrap">
            <el-pagination
              background
              layout="total, sizes, prev, pager, next"
              :total="filteredTasks.length"
              :page-size="pager.pageSize"
              :current-page="pager.page"
              :page-sizes="[20, 50, 100]"
              @size-change="onPageSizeChange"
              @current-change="onPageChange"
            />
          </div>
        </el-card>
      </section>
    </section>

    <el-dialog
      v-model="formVisible"
      :title="formMode === 'create' ? '新建任务' : `编辑任务 #${form.id}`"
      width="920px"
    >
      <el-form :model="form" label-width="92px">
        <el-row :gutter="12">
          <el-col :span="12"><el-form-item label="任务名"><el-input v-model="form.name" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="Cluster Key"><el-input v-model="form.cluster_key" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="主机"><el-input v-model="form.source.host" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="端口"><el-input-number v-model="form.source.port" :min="1" :max="65535" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="用户"><el-input v-model="form.source.user" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="密码"><el-input v-model="form.source.password" show-password placeholder="编辑时留空=不修改" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="Server ID"><el-input-number v-model="form.source.server_id" :min="1" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="Flavor"><el-input v-model="form.source.flavor" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="SemiSync"><el-switch v-model="form.source.semi_sync" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="保留天数"><el-input-number v-model="form.storage.retention_days" :min="1" /></el-form-item></el-col>
          <el-col :span="8">
            <el-form-item label="起点模式">
              <el-select v-model="form.start.mode" style="width: 100%">
                <el-option value="LATEST" label="LATEST" />
                <el-option value="FILE_POS" label="FILE_POS" />
                <el-option value="GTID" label="GTID" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="8"><el-form-item label="文件"><el-input v-model="form.start.file" /></el-form-item></el-col>
          <el-col :span="8"><el-form-item label="位置"><el-input-number v-model="form.start.pos" :min="0" /></el-form-item></el-col>
          <el-col :span="24"><el-form-item label="GTID"><el-input v-model="form.start.gtid_set" /></el-form-item></el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="formVisible = false">取消</el-button>
        <el-button type="primary" @click="submitForm">保存</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="batchVisible" title="批量创建任务" width="920px">
      <el-alert type="info" show-icon :closable="false">
        <p>每行一条，支持三种格式：</p>
        <p><code>name,host,port</code> 或 <code>host,port</code> 或 <code>host:port</code></p>
      </el-alert>
      <el-row :gutter="12" class="batch-grid">
        <el-col :span="12">
          <el-form label-width="98px">
            <el-form-item label="复制用户">
              <el-input v-model="batchForm.user" />
            </el-form-item>
            <el-form-item label="复制密码">
              <el-input v-model="batchForm.password" show-password />
            </el-form-item>
            <el-form-item label="Flavor">
              <el-input v-model="batchForm.flavor" />
            </el-form-item>
            <el-form-item label="起始 ServerID">
              <el-input-number v-model="batchForm.serverIdStart" :min="1" />
            </el-form-item>
            <el-form-item label="保留天数">
              <el-input-number v-model="batchForm.retentionDays" :min="1" />
            </el-form-item>
            <el-form-item label="SemiSync">
              <el-switch v-model="batchForm.semiSync" />
            </el-form-item>
            <el-form-item label="创建后启动">
              <el-switch v-model="batchForm.autoStart" />
            </el-form-item>
          </el-form>
        </el-col>
        <el-col :span="12">
          <el-input
            v-model="batchForm.lines"
            type="textarea"
            :rows="14"
            placeholder="示例：
e2e-mysql57,127.0.0.1,13306
e2e-mysql80,127.0.0.1,13307
127.0.0.1:13308"
          />
        </el-col>
        <el-col :span="24">
          <div class="batch-preview-toolbar">
            <div class="batch-preview-summary">
              <span>预览状态：</span>
              <el-tag v-if="batchPreview.ready && batchPreview.errors.length === 0" size="small" type="success">可提交</el-tag>
              <el-tag v-else-if="batchPreview.ready" size="small" type="danger">有错误</el-tag>
              <el-tag v-else size="small" type="info">未预览</el-tag>
              <span class="batch-preview-count">有效 {{ batchPreview.validCount }} / 错误 {{ batchPreview.errors.length }}</span>
            </div>
            <div class="batch-preview-actions">
              <el-button @click="previewBatchCreate">预览校验</el-button>
              <el-button @click="clearBatchPreview">清空预览</el-button>
            </div>
          </div>

          <el-table v-if="batchPreview.rows.length" :data="batchPreview.rows" size="small" border class="batch-preview-table">
            <el-table-column prop="lineNo" label="行号" width="80" />
            <el-table-column prop="name" label="任务名" min-width="180" />
            <el-table-column prop="host" label="Host" min-width="160" />
            <el-table-column prop="port" label="Port" width="100" />
            <el-table-column label="校验" width="110">
              <template #default="{ row }">
                <el-tag size="small" :type="row.valid ? 'success' : 'danger'">{{ row.valid ? "通过" : "失败" }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column prop="error" label="原因" min-width="220" />
          </el-table>
        </el-col>
      </el-row>
      <template #footer>
        <el-button @click="batchVisible = false">取消</el-button>
        <el-button type="primary" :disabled="!batchPreview.canSubmit" @click="submitBatchCreate">开始批量创建</el-button>
      </template>
    </el-dialog>

    <el-drawer v-model="detailVisible" size="66%" :title="detailTask ? `任务详情 #${detailTask.id}` : '任务详情'">
      <template v-if="detailTask">
        <div class="detail-stack">
          <section class="detail-panel">
            <h3><i class="fa-solid fa-circle-info" /> 基础信息</h3>
            <div class="detail-grid">
              <div class="detail-item"><span>名称</span><strong>{{ detailTask.name }}</strong></div>
              <div class="detail-item"><span>Cluster Key</span><strong>{{ detailTask.cluster_key || "--" }}</strong></div>
              <div class="detail-item"><span>状态</span><strong>{{ detailTask.state }}</strong></div>
              <div class="detail-item"><span>源库</span><strong>{{ sourceLabel(detailTask) }}</strong></div>
              <div class="detail-item"><span>起点</span><strong>{{ detailTask.start?.mode }}</strong></div>
              <div class="detail-item"><span>SemiSync</span><strong>{{ detailTask.source?.semi_sync ? "ON" : "OFF" }}</strong></div>
              <div class="detail-item"><span>保留天数</span><strong>{{ detailTask.storage?.retention_days }}</strong></div>
            </div>
          </section>

          <section class="detail-panel" v-if="detailReplication">
            <h3><i class="fa-solid fa-wave-square" /> 复制状态</h3>
            <div class="detail-grid">
              <div class="detail-item">
                <span>状态</span>
                <strong><el-tag :type="replicationTagType(detailReplication.status)">{{ detailReplication.status }}</el-tag></strong>
              </div>
              <div class="detail-item"><span>延迟(s)</span><strong>{{ formatDelay(detailReplication.delay_seconds, detailReplication.has_progress) }}</strong></div>
              <div class="detail-item"><span>原因</span><strong>{{ formatReplicationReason(detailReplication) }}</strong></div>
              <div class="detail-item"><span>Reason Code</span><strong>{{ detailReplication.reason || "--" }}</strong></div>
              <div class="detail-item"><span>阈值(s)</span><strong>{{ detailReplication.threshold_seconds || "--" }}</strong></div>
              <div class="detail-item"><span>最近事件时间</span><strong>{{ formatTs(detailReplication.last_event_at) }}</strong></div>
              <div class="detail-item"><span>最近位点</span><strong>{{ detailReplication.last_event_file || "-" }}:{{ detailReplication.last_event_pos || 0 }}</strong></div>
            </div>
          </section>

          <section class="detail-panel" v-if="detailLease">
            <h3><i class="fa-solid fa-key" /> Cluster Lease</h3>
            <div class="detail-grid">
              <div class="detail-item"><span>归属 Worker</span><strong>{{ detailLease.owner_worker_id || "--" }}</strong></div>
              <div class="detail-item"><span>Epoch</span><strong>{{ detailLease.epoch || "--" }}</strong></div>
              <div class="detail-item"><span>Lease 风险</span><strong>{{ leaseRiskLabel(detailTask, detailLease) }}</strong></div>
              <div class="detail-item"><span>更新时间</span><strong>{{ formatTs(detailLease.updated_at) }}</strong></div>
            </div>
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-clock-rotate-left" /> Run History（最近 {{ runHistoryLimit }} 条）</h3>
            <el-table :data="detailRunsLimited" size="small" border>
              <el-table-column prop="run_id" label="Run ID" min-width="180" />
              <el-table-column prop="worker_id" label="Worker" min-width="120" />
              <el-table-column prop="epoch" label="Epoch" width="100" />
              <el-table-column label="Started At" min-width="170">
                <template #default="{ row }">{{ formatTs(row.started_at) }}</template>
              </el-table-column>
              <el-table-column label="Ended At" min-width="170">
                <template #default="{ row }">{{ formatTs(row.ended_at) }}</template>
              </el-table-column>
              <el-table-column label="End Reason" min-width="140">
                <template #default="{ row }">{{ row.end_reason || "--" }}</template>
              </el-table-column>
            </el-table>
            <el-empty v-if="detailRunsLimited.length === 0" description="暂无 run history" :image-size="56" />
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-location-dot" /> Checkpoint</h3>
            <div class="checkpoint">{{ formatCheckpoint(checkpoint) }}</div>
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-file-lines" /> 文件元数据</h3>
            <el-table :data="files" size="small" border>
              <el-table-column prop="file_name" label="File" min-width="180" />
              <el-table-column prop="size_bytes" label="Size" width="100" />
              <el-table-column prop="start_pos" label="Start" width="90" />
              <el-table-column prop="end_pos" label="End" width="90" />
              <el-table-column prop="upload_state" label="Upload" width="130" />
              <el-table-column prop="object_key" label="ObjectKey" min-width="190" />
            </el-table>
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-clock-rotate-left" /> 事件</h3>
            <el-timeline>
              <el-timeline-item
                v-for="ev in events"
                :key="`${ev.sequence}-${ev.type}`"
                :timestamp="ev.time"
              >
                <strong>{{ ev.type }}</strong>
                <p>{{ ev.message }}</p>
              </el-timeline-item>
            </el-timeline>
          </section>
        </div>
      </template>
    </el-drawer>
  </div>
</template>

<script setup>
import { computed, reactive, ref, watch } from "vue";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  createTask,
  deleteTask,
  getClusterOverview,
  getCheckpoint,
  getDashboard,
  getReplication,
  getTaskLease,
  getTask,
  listTaskRuns,
  listEvents,
  listFiles,
  listWorkers,
  lookupSource,
  startTask,
  stopTask,
  updateTask,
} from "./api";

const loading = ref(false);
const LEASE_RISK_SECONDS = 45;
const RUN_HISTORY_LIMIT = 10;
const NAME_MAX_LENGTH = 255;
const CLUSTER_KEY_PATTERN = /^[A-Za-z0-9._-]+$/;
const SOURCE_HOST_MAX_LENGTH = 255;
const SOURCE_USER_MAX_LENGTH = 128;
const SOURCE_FLAVOR_MAX_LENGTH = 32;
const START_FILE_MAX_LENGTH = 255;
const RETENTION_DAYS_MIN = 1;
const RETENTION_DAYS_MAX = 3650;
const dashboard = reactive({
  generated_at: "",
  threshold_seconds: 30,
  summary: {
    total: 0,
    running: 0,
    retry_backoff: 0,
    stopped: 0,
    failed: 0,
    normal: 0,
    delayed: 0,
    abnormal: 0,
  },
  tasks: [],
  sources: [],
});

const cluster = reactive({
  overview: {
    task_count: 0,
    worker_count: 0,
    running_task_count: 0,
    leased_task_count: 0,
  },
  workers: [],
  leaseByTask: {},
});

const sourceQuery = reactive({
  host: "",
  port: null,
});

const lookup = reactive({
  checked: false,
  exists: false,
  count: 0,
});

const uiFilter = reactive({
  keyword: "",
  sourceKeyword: "",
  taskState: "ALL",
  replicationStatus: "ALL",
  sortBy: "delay_desc",
  onlyAlert: false,
});

const pager = reactive({
  page: 1,
  pageSize: 20,
});

const taskStates = ["CREATED", "STARTING", "RUNNING", "RETRY_BACKOFF", "STOPPING", "STOPPED", "FAILED"];
const replicationStatuses = ["NORMAL", "DELAYED", "ABNORMAL", "IDLE"];

const formVisible = ref(false);
const formMode = ref("create");
const form = reactive(defaultForm());
const batchVisible = ref(false);
const batchForm = reactive(defaultBatchForm());
const batchPreview = reactive({
  ready: false,
  canSubmit: false,
  validCount: 0,
  rows: [],
  errors: [],
});

const detailVisible = ref(false);
const detailTask = ref(null);
const detailReplication = ref(null);
const detailLease = ref(null);
const detailRuns = ref([]);
const runHistoryLimit = ref(RUN_HISTORY_LIMIT);
const checkpoint = ref(null);
const events = ref([]);
const files = ref([]);

const filteredTasks = computed(() => {
  let rows = [...dashboard.tasks];

  if (uiFilter.keyword.trim()) {
    const kw = uiFilter.keyword.trim().toLowerCase();
    rows = rows.filter((row) => {
      const id = String(row.task?.id || "").toLowerCase();
      const name = String(row.task?.name || "").toLowerCase();
      return id.includes(kw) || name.includes(kw);
    });
  }

  if (uiFilter.sourceKeyword.trim()) {
    const sourceKw = uiFilter.sourceKeyword.trim().toLowerCase();
    rows = rows.filter((row) => sourceLabel(row.task).toLowerCase().includes(sourceKw));
  }

  if (uiFilter.taskState !== "ALL") {
    rows = rows.filter((row) => row.task?.state === uiFilter.taskState);
  }

  if (uiFilter.replicationStatus !== "ALL") {
    rows = rows.filter((row) => row.replication?.status === uiFilter.replicationStatus);
  }

  if (uiFilter.onlyAlert) {
    rows = rows.filter((row) => {
      const rep = row.replication?.status;
      const taskState = row.task?.state;
      return rep === "ABNORMAL" || rep === "DELAYED" || taskState === "FAILED" || taskState === "RETRY_BACKOFF";
    });
  }

  rows.sort((a, b) => {
    if (uiFilter.sortBy === "name_asc") {
      return String(a.task?.name || "").localeCompare(String(b.task?.name || ""));
    }
    if (uiFilter.sortBy === "updated_desc") {
      const at = new Date(a.task?.updated_at || 0).getTime();
      const bt = new Date(b.task?.updated_at || 0).getTime();
      return bt - at;
    }

    const ad = Number(a.replication?.delay_seconds ?? -1);
    const bd = Number(b.replication?.delay_seconds ?? -1);
    return bd - ad;
  });

  return rows;
});

const pagedTasks = computed(() => {
  const start = (pager.page - 1) * pager.pageSize;
  return filteredTasks.value.slice(start, start + pager.pageSize);
});

const workerRows = computed(() => {
  return (cluster.workers || []).map((worker) => {
    const seenAt = worker.last_seen_at || worker.updated_at;
    const fallbackOnline = (() => {
      const updatedMs = toTimeMs(seenAt);
      return updatedMs > 0 && Date.now() - updatedMs <= LEASE_RISK_SECONDS * 1000;
    })();
    const online = typeof worker.online === "boolean" ? worker.online : fallbackOnline;
    return {
      ...worker,
      last_seen_at: seenAt,
      online,
    };
  });
});

const detailRunsLimited = computed(() => detailRuns.value.slice(0, runHistoryLimit.value));

watch(
  () => [
    uiFilter.keyword,
    uiFilter.sourceKeyword,
    uiFilter.taskState,
    uiFilter.replicationStatus,
    uiFilter.sortBy,
    uiFilter.onlyAlert,
    dashboard.tasks.length,
  ],
  () => {
    pager.page = 1;
  },
);

watch(
  () => [filteredTasks.value.length, pager.pageSize],
  () => {
    const maxPage = Math.max(1, Math.ceil(filteredTasks.value.length / pager.pageSize));
    if (pager.page > maxPage) pager.page = maxPage;
  },
);

watch(
  () => batchForm.lines,
  () => {
    if (!batchVisible.value || !batchPreview.ready) return;
    clearBatchPreview();
  },
);

watch(
  () => pagedTasks.value.map((row) => row.task?.id || "").join(","),
  () => {
    void prefetchTaskLeasesForPage();
  },
  { immediate: true },
);

refreshAll();

async function refreshAll() {
  try {
    loading.value = true;
    const [dashboardData, overviewData, workersData] = await Promise.all([
      getDashboard(buildSourceFilter()),
      getClusterOverview(),
      listWorkers(),
    ]);
    applyDashboardData(dashboardData);
    applyClusterData(overviewData, workersData);
    await prefetchTaskLeasesForPage();
  } catch (err) {
    ElMessage.error(parseErr(err));
  } finally {
    loading.value = false;
  }
}

function buildSourceFilter() {
  const params = {};
  if (sourceQuery.host?.trim()) params.host = sourceQuery.host.trim();
  if (sourceQuery.port) params.port = Number(sourceQuery.port);
  return params;
}

function applyDashboardData(data) {
  Object.assign(dashboard.summary, data?.summary || {});
  dashboard.tasks = data?.tasks || [];
  dashboard.sources = data?.sources || [];
  dashboard.generated_at = data?.generated_at || "";
  dashboard.threshold_seconds = Number(data?.threshold_seconds || 30);
}

function applyClusterData(overview, workers) {
  cluster.overview.task_count = Number(overview?.task_count || 0);
  cluster.overview.worker_count = Number(overview?.worker_count || 0);
  cluster.overview.running_task_count = Number(overview?.running_task_count || 0);
  cluster.overview.leased_task_count = Number(overview?.leased_task_count || 0);
  cluster.workers = Array.isArray(workers) ? workers : [];
}

async function prefetchTaskLeasesForPage() {
  const ids = pagedTasks.value.map((row) => row.task?.id).filter(Boolean);
  if (!ids.length) return;

  const results = await Promise.allSettled(ids.map((id) => getTaskLease(id)));
  results.forEach((result, idx) => {
    const id = ids[idx];
    if (result.status === "fulfilled") {
      cluster.leaseByTask[id] = result.value;
    }
  });
}

async function applySourceFilter() {
  const params = buildSourceFilter();
  if (!params.host || !params.port) {
    ElMessage.error("请输入 host 和 port 再查询");
    return;
  }

  try {
    loading.value = true;
    const [lookupResp, dashboardResp, overviewResp, workersResp] = await Promise.all([
      lookupSource(params),
      getDashboard(params),
      getClusterOverview(),
      listWorkers(),
    ]);
    lookup.checked = true;
    lookup.exists = !!lookupResp?.exists;
    lookup.count = Number(lookupResp?.count || 0);
    applyDashboardData(dashboardResp);
    applyClusterData(overviewResp, workersResp);
    await prefetchTaskLeasesForPage();
  } catch (err) {
    ElMessage.error(parseErr(err));
  } finally {
    loading.value = false;
  }
}

async function clearSourceFilter() {
  sourceQuery.host = "";
  sourceQuery.port = null;
  lookup.checked = false;
  lookup.exists = false;
  lookup.count = 0;
  await refreshAll();
}

function resetUiFilter() {
  uiFilter.keyword = "";
  uiFilter.sourceKeyword = "";
  uiFilter.taskState = "ALL";
  uiFilter.replicationStatus = "ALL";
  uiFilter.sortBy = "delay_desc";
  uiFilter.onlyAlert = false;
}

function onPageChange(page) {
  pager.page = page;
}

function onPageSizeChange(size) {
  pager.pageSize = size;
}

function defaultForm() {
  return {
    id: "",
    name: "",
    cluster_key: "",
    source: {
      host: "127.0.0.1",
      port: 3306,
      user: "repl",
      password: "",
      flavor: "mysql",
      server_id: 200001,
      semi_sync: false,
    },
    start: {
      mode: "LATEST",
      file: "",
      pos: 0,
      gtid_set: "",
    },
    storage: {
      retention_days: 7,
    },
  };
}

function defaultBatchForm() {
  return {
    lines: "",
    user: "repl",
    password: "replpass",
    flavor: "mysql",
    serverIdStart: 300000,
    retentionDays: 7,
    semiSync: false,
    autoStart: false,
  };
}

function clearBatchPreview() {
  batchPreview.ready = false;
  batchPreview.canSubmit = false;
  batchPreview.validCount = 0;
  batchPreview.rows = [];
  batchPreview.errors = [];
}

function resetBatchForm() {
  Object.assign(batchForm, defaultBatchForm());
  clearBatchPreview();
}

function resetForm() {
  Object.assign(form, defaultForm());
}

function openCreate() {
  formMode.value = "create";
  resetForm();
  formVisible.value = true;
}

function openBatchCreate() {
  resetBatchForm();
  batchVisible.value = true;
}

function openEdit(task) {
  formMode.value = "edit";
  Object.assign(form, defaultForm(), JSON.parse(JSON.stringify(task)));
  form.source.password = "";
  formVisible.value = true;
}

function buildPayload() {
  const payload = {
    name: form.name.trim(),
    cluster_key: form.cluster_key?.trim(),
    source: {
      ...form.source,
      host: form.source.host?.trim() || "",
      user: form.source.user?.trim() || "",
      flavor: form.source.flavor?.trim() || "",
      semi_sync: !!form.source.semi_sync,
    },
    start: { mode: form.start.mode },
    storage: { retention_days: Number(form.storage.retention_days) },
  };
  if (!payload.source.password) delete payload.source.password;

  if (payload.start.mode === "FILE_POS") {
    payload.start.file = form.start.file?.trim() || "";
    payload.start.pos = Number(form.start.pos || 0);
  }
  if (payload.start.mode === "GTID") {
    payload.start.gtid_set = form.start.gtid_set?.trim() || "";
  }
  return payload;
}

function parseBatchLine(raw, lineNo) {
  const parts = raw.split(",").map((x) => x.trim()).filter(Boolean);
  let name = "";
  let host = "";
  let port = 0;

  if (parts.length === 1) {
    if (!parts[0].includes(":")) throw new Error(`第 ${lineNo} 行格式错误，单列必须是 host:port`);
    [host, port] = parseHostPort(parts[0], lineNo);
    name = `task-${host}-${port}`;
  } else if (parts.length === 2) {
    if (parts[1].includes(":")) {
      name = parts[0];
      [host, port] = parseHostPort(parts[1], lineNo);
    } else if (parts[0].includes(":")) {
      [host, port] = parseHostPort(parts[0], lineNo);
      name = parts[1];
    } else {
      host = parts[0];
      port = Number(parts[1]);
      name = `task-${host}-${port}`;
    }
  } else if (parts.length === 3) {
    name = parts[0];
    host = parts[1];
    port = Number(parts[2]);
  } else {
    throw new Error(`第 ${lineNo} 行格式错误，只支持 name,host,port / host,port / host:port`);
  }

  if (!host || !Number.isInteger(Number(port)) || Number(port) <= 0 || Number(port) > 65535) {
    throw new Error(`第 ${lineNo} 行端口不合法`);
  }
  if (!name) {
    name = `task-${host}-${port}`;
  }
  const clusterKey = makeClusterKey(name, host, Number(port), lineNo);
  const clusterKeyErr = validateClusterKey(clusterKey);
  if (clusterKeyErr) {
    throw new Error(`第 ${lineNo} 行 cluster_key 不合法：${clusterKeyErr}`);
  }

  return {
    lineNo,
    name,
    host,
    port: Number(port),
    clusterKey,
  };
}

function parseBatchLines(rawText) {
  const rows = [];
  const errors = [];
  const sourceSeen = new Set();
  const nameSeen = new Set();
  const clusterKeySeen = new Set();

  const lines = rawText.split("\n");
  for (let i = 0; i < lines.length; i += 1) {
    const lineNo = i + 1;
    const raw = lines[i].trim();
    if (!raw || raw.startsWith("#")) continue;

    try {
      const row = parseBatchLine(raw, lineNo);
      const sourceKey = `${row.host}:${row.port}`;
      const nameKey = row.name.toLowerCase();
      if (sourceSeen.has(sourceKey)) {
        throw new Error(`第 ${lineNo} 行源库重复：${sourceKey}`);
      }
      if (nameSeen.has(nameKey)) {
        throw new Error(`第 ${lineNo} 行任务名重复：${row.name}`);
      }
      if (clusterKeySeen.has(row.clusterKey)) {
        throw new Error(`第 ${lineNo} 行 cluster_key 重复：${row.clusterKey}`);
      }
      sourceSeen.add(sourceKey);
      nameSeen.add(nameKey);
      clusterKeySeen.add(row.clusterKey);
      rows.push({ ...row, valid: true, error: "" });
    } catch (err) {
      const msg = err?.message || `第 ${lineNo} 行格式错误`;
      rows.push({
        lineNo,
        name: "--",
        host: "--",
        port: "--",
        valid: false,
        error: msg,
      });
      errors.push(msg);
    }
  }
  if (!rows.length) {
    errors.push("没有可解析的数据行");
  }
  return { rows, errors };
}

function parseHostPort(text, lineNo) {
  const idx = text.lastIndexOf(":");
  if (idx <= 0 || idx === text.length - 1) throw new Error(`第 ${lineNo} 行 host:port 格式错误`);
  const host = text.slice(0, idx).trim();
  const port = Number(text.slice(idx + 1).trim());
  if (!host || !Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error(`第 ${lineNo} 行 host:port 格式错误`);
  }
  return [host, port];
}

function previewBatchCreate() {
  const parsed = parseBatchLines(batchForm.lines || "");
  batchPreview.rows = parsed.rows;
  batchPreview.errors = parsed.errors;
  batchPreview.validCount = parsed.rows.filter((x) => x.valid).length;
  batchPreview.ready = true;
  batchPreview.canSubmit = batchPreview.validCount > 0 && parsed.errors.length === 0;

  if (!batchPreview.rows.length || parsed.errors.length > 0) {
    ElMessage.warning(`预览完成：有效 ${batchPreview.validCount}，错误 ${parsed.errors.length}`);
    return;
  }
  ElMessage.success(`预览通过：共 ${batchPreview.validCount} 条`);
}

async function submitBatchCreate() {
  try {
    if (!batchPreview.ready) {
      ElMessage.warning("请先点击“预览校验”");
      return;
    }
    if (!batchPreview.canSubmit) {
      ElMessage.error("预览未通过，请修正后重试");
      return;
    }
    if (!batchForm.user.trim()) {
      ElMessage.error("复制用户不能为空");
      return;
    }
    const rows = batchPreview.rows.filter((x) => x.valid);

    let success = 0;
    const errors = [];
    for (let i = 0; i < rows.length; i += 1) {
      const row = rows[i];
      const source = {
        host: row.host,
        port: row.port,
        user: batchForm.user.trim(),
        flavor: batchForm.flavor?.trim() || "mysql",
        server_id: Number(batchForm.serverIdStart) + i,
        semi_sync: !!batchForm.semiSync,
      };
      if (batchForm.password) {
        source.password = batchForm.password;
      }

      const payload = {
        name: row.name,
        cluster_key: row.clusterKey,
        source,
        start: { mode: "LATEST" },
        storage: { retention_days: Number(batchForm.retentionDays) },
      };
      const validationErr = validateTaskPayload(payload);
      if (validationErr) {
        errors.push(`${row.name}(${row.host}:${row.port}) -> ${validationErr}`);
        continue;
      }

      try {
        const created = await createTask(payload);
        if (batchForm.autoStart) {
          await startTask(created.id);
        }
        success += 1;
      } catch (err) {
        errors.push(`${row.name}(${row.host}:${row.port}) -> ${parseErr(err)}`);
      }
    }

    await refreshAll();
    if (!errors.length) {
      ElMessage.success(`批量创建成功，共 ${success} 个任务`);
      batchVisible.value = false;
      return;
    }

    ElMessage.warning(`成功 ${success} 个，失败 ${errors.length} 个`);
    await ElMessageBox.alert(`<pre style="white-space: pre-wrap">${errors.join("\n")}</pre>`, "批量创建失败明细", {
      dangerouslyUseHTMLString: true,
      confirmButtonText: "知道了",
    });
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function submitForm() {
  try {
    const payload = buildPayload();
    const validationErr = validateTaskPayload(payload);
    if (validationErr) {
      ElMessage.error(validationErr);
      return;
    }
    if (formMode.value === "create") {
      await createTask(payload);
      ElMessage.success("任务已创建");
    } else {
      await updateTask(form.id, payload);
      ElMessage.success("任务已更新");
    }
    formVisible.value = false;
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onStart(task) {
  try {
    await startTask(task.id);
    ElMessage.success(`任务 #${task.id} 已启动`);
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onStop(task) {
  try {
    await stopTask(task.id);
    ElMessage.success(`任务 #${task.id} 已停止`);
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onDelete(task) {
  try {
    await ElMessageBox.confirm(`确认删除任务 #${task.id} ?`, "删除确认", { type: "warning" });
    await deleteTask(task.id);
    ElMessage.success(`任务 #${task.id} 已删除`);
    await refreshAll();
  } catch (err) {
    if (err !== "cancel") ElMessage.error(parseErr(err));
  }
}

function onRowClick(row) {
  showDetail(row.task);
}

async function showDetail(taskOrID) {
  try {
    const id = typeof taskOrID === "string" ? taskOrID : taskOrID.id;
    const [task, cp, evs, fs, replication] = await Promise.all([
      getTask(id),
      getCheckpoint(id),
      listEvents(id, 120),
      listFiles(id, 80),
      getReplication(id),
    ]);
    const [leaseResult, runsResult] = await Promise.allSettled([
      getTaskLease(id),
      listTaskRuns(id, RUN_HISTORY_LIMIT),
    ]);
    const lease = leaseResult.status === "fulfilled" ? leaseResult.value : null;
    const runs = runsResult.status === "fulfilled" ? runsResult.value : [];

    detailTask.value = task;
    detailReplication.value = replication;
    detailLease.value = lease;
    detailRuns.value = Array.isArray(runs) ? runs : [];
    runHistoryLimit.value = RUN_HISTORY_LIMIT;
    checkpoint.value = cp;
    events.value = evs || [];
    files.value = fs || [];
    if (lease) {
      cluster.leaseByTask[id] = lease;
    }
    detailVisible.value = true;
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

function sourceLabel(task) {
  return `${task?.source?.host || "-"}:${task?.source?.port || "-"}`;
}

function ownerWorkerLabel(task) {
  const leaseWorker = cluster.leaseByTask[task?.id]?.owner_worker_id;
  return task?.owner_worker_id || leaseWorker || "--";
}

function leaseRiskTagType(task, leaseOverride = null) {
  const label = leaseRiskLabel(task, leaseOverride);
  if (label === "正常") return "success";
  if (label === "风险") return "warning";
  return "info";
}

function leaseRiskLabel(task, leaseOverride = null) {
  const lease = leaseOverride || cluster.leaseByTask[task?.id];
  const owner = lease?.owner_worker_id || task?.owner_worker_id;
  const epoch = Number(lease?.epoch ?? task?.epoch ?? 0);
  if (!owner || epoch <= 0) return "--";

  const updatedMs = toTimeMs(lease?.updated_at || task?.updated_at);
  if (updatedMs <= 0) return "风险";
  const stale = Date.now() - updatedMs > LEASE_RISK_SECONDS * 1000;
  return stale ? "风险" : "正常";
}

function stateTagType(state) {
  if (state === "RUNNING") return "success";
  if (state === "RETRY_BACKOFF") return "warning";
  if (state === "FAILED") return "danger";
  return "info";
}

function replicationTagType(status) {
  if (status === "NORMAL") return "success";
  if (status === "DELAYED") return "warning";
  if (status === "ABNORMAL") return "danger";
  return "info";
}

function formatDelay(delaySeconds, hasProgress) {
  if (!hasProgress || delaySeconds === undefined || delaySeconds === null) {
    return "--";
  }
  return String(delaySeconds);
}

function formatTs(ts) {
  if (!ts) return "--";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "--";
  return date.toLocaleString();
}

function toTimeMs(ts) {
  if (!ts) return 0;
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return 0;
  return date.getTime();
}

function formatCheckpoint(cp) {
  if (!cp) return "暂无";
  const file = cp.file || cp.File || cp.file_name || cp.FileName || cp.binlog_file || cp.BinlogFile || "-";
  const pos = cp.pos ?? cp.Pos ?? cp.position ?? cp.Position ?? cp.binlog_pos ?? cp.BinlogPos ?? 0;
  return `${file}:${pos}`;
}

function formatReplicationReason(rep) {
  if (!rep) return "--";
  const reasonMap = {
    NO_PROGRESS: "未收到复制事件",
    DELAY_EXCEEDS_THRESHOLD: "延迟超过阈值",
    RUNNER_ERROR: "复制错误",
    TASK_STATE_ERROR: "任务状态异常",
  };
  const rawErr = rep.last_error || rep.error || rep.err || rep.message || "";
  if (rawErr) {
    const label = reasonMap[rep.reason] || rep.reason || "复制错误";
    return `${label}: ${rawErr}`;
  }
  if (rep.reason) return reasonMap[rep.reason] || rep.reason;
  if (rep.status === "DELAYED") return "延迟超过阈值";
  if (rep.status === "ABNORMAL") return "复制异常（暂无详细错误）";
  return "--";
}

function hasReplicationReason(rep) {
  return formatReplicationReason(rep) !== "--";
}

function parseErr(err) {
  if (typeof err?.response?.data === "string") return err.response.data;
  if (err?.response?.data) return JSON.stringify(err.response.data);
  return err?.message || String(err);
}

function makeClusterKey(name, host, port, lineNo = 0) {
  const raw = `${name}-${host}-${port}-${lineNo}`.toLowerCase();
  const key = raw
    .replace(/[^a-z0-9._-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
  return key || `cluster-${Date.now()}-${Math.floor(Math.random() * 1000)}`;
}

function validateClusterKey(clusterKeyRaw) {
  const clusterKey = String(clusterKeyRaw || "").trim();
  if (!clusterKey) {
    return "cluster_key 不能为空";
  }
  if (
    clusterKey.includes("/") ||
    clusterKey.includes("\\") ||
    clusterKey.includes("..") ||
    !CLUSTER_KEY_PATTERN.test(clusterKey)
  ) {
    return "cluster_key 不合法（仅允许字母数字._-，禁止 / \\\\ ..）";
  }
  return "";
}

function validateTaskPayload(payload) {
  const name = String(payload?.name || "").trim();
  if (!name || name.length > NAME_MAX_LENGTH) {
    return "任务名不合法（1-255 字符）";
  }

  const clusterKeyErr = validateClusterKey(payload?.cluster_key);
  if (clusterKeyErr) {
    return clusterKeyErr;
  }

  const source = payload?.source || {};
  const host = String(source.host || "").trim();
  const user = String(source.user || "").trim();
  const flavorRaw = String(source.flavor || "").trim();
  const flavor = flavorRaw || "mysql";
  const port = Number(source.port || 0);
  const serverID = Number(source.server_id || 0);

  if (!host || host.length > SOURCE_HOST_MAX_LENGTH || hasWhitespace(host)) {
    return "source.host 不合法";
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return "source.port 不合法（1-65535）";
  }
  if (!user || user.length > SOURCE_USER_MAX_LENGTH || hasWhitespace(user)) {
    return "source.user 不合法";
  }
  if (!flavor || flavor.length > SOURCE_FLAVOR_MAX_LENGTH || !CLUSTER_KEY_PATTERN.test(flavor)) {
    return "source.flavor 不合法";
  }
  if (!Number.isInteger(serverID) || serverID < 0 || serverID > 4294967295) {
    return "source.server_id 不合法（0 或 1..4294967295）";
  }

  const start = payload?.start || {};
  const mode = String(start.mode || "").trim();
  if (!["LATEST", "FILE_POS", "GTID"].includes(mode)) {
    return "start.mode 不合法";
  }
  if (mode === "FILE_POS") {
    const file = String(start.file || "").trim();
    const pos = Number(start.pos || 0);
    if (!file || file.length > START_FILE_MAX_LENGTH || !Number.isInteger(pos) || pos <= 0) {
      return "FILE_POS 模式要求合法 file/pos";
    }
  }
  if (mode === "GTID") {
    const gtidSet = String(start.gtid_set || "").trim();
    if (!gtidSet) {
      return "GTID 模式要求 gtid_set";
    }
  }

  const retentionDays = Number(payload?.storage?.retention_days || 0);
  if (!Number.isInteger(retentionDays) || retentionDays < RETENTION_DAYS_MIN || retentionDays > RETENTION_DAYS_MAX) {
    return "storage.retention_days 不合法（1-3650）";
  }

  return "";
}

function hasWhitespace(text) {
  return /\s/.test(String(text || ""));
}
</script>

<style scoped>
@import url("https://fonts.cdnfonts.com/css/geist");
@import url("https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500&display=swap");

.page-shell {
  --bg: #fafafa;
  --surface: #ffffff;
  --surface-soft: #fcfcfc;
  --line: #ebebeb;
  --line-strong: #dbdbdb;
  --text: #111111;
  --sub: #666666;
  --accent: #111111;

  max-width: 1720px;
  margin: 0 auto;
  min-height: 100vh;
  padding: 24px;
  font-family: "Geist", "SF Pro Display", "PingFang SC", sans-serif;
  color: var(--text);
  background:
    radial-gradient(circle at 0% 0%, #ffffff 0, #fafafa 42%),
    linear-gradient(90deg, rgba(0, 0, 0, 0.015) 1px, transparent 1px),
    linear-gradient(rgba(0, 0, 0, 0.015) 1px, transparent 1px);
  background-size: auto, 28px 28px, 28px 28px;
}

.orb {
  display: none;
}

.hero {
  display: flex;
  justify-content: space-between;
  align-items: flex-end;
  gap: 16px;
  margin-bottom: 18px;
  animation: rise-fade 0.36s ease both;
}

.hero-copy {
  max-width: 900px;
}

.kicker {
  margin: 0;
  color: var(--sub);
  letter-spacing: 0.12em;
  font-size: 11px;
  font-weight: 600;
}

.kicker i {
  margin-right: 6px;
}

h1 {
  margin: 6px 0 4px;
  font-size: 40px;
  line-height: 1.06;
  letter-spacing: -0.03em;
  font-weight: 700;
}

.hero-desc {
  margin: 0;
  color: var(--sub);
}

.hero-actions {
  display: flex;
  gap: 8px;
}

.hero-actions i {
  margin-right: 6px;
}

.hero-actions :deep(.el-button) {
  height: 36px;
  font-weight: 600;
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(6, minmax(130px, 1fr));
  gap: 10px;
  margin-bottom: 14px;
}

.metric-card {
  border: 1px solid var(--line);
  background: var(--surface);
  border-radius: 12px;
  padding: 12px;
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.03);
  animation: rise-fade 0.42s ease both;
}

.metric-card p {
  margin: 0;
  color: var(--sub);
  font-size: 12px;
  display: flex;
  align-items: center;
  gap: 6px;
}

.metric-card i {
  color: #525252;
}

.metric-card strong {
  display: block;
  margin-top: 10px;
  font-size: 32px;
  line-height: 1;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
}

.workspace {
  display: grid;
  grid-template-columns: 320px minmax(0, 1fr);
  gap: 12px;
}

.left-pane,
.right-pane {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.panel-card {
  border-radius: 14px;
  border: 1px solid var(--line);
  background: var(--surface);
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.03);
  transition:
    transform 0.2s ease,
    box-shadow 0.2s ease,
    border-color 0.2s ease;
  animation: rise-fade 0.5s ease both;
}

.panel-card:hover {
  transform: translateY(-1px);
  border-color: var(--line-strong);
  box-shadow: 0 3px 8px rgba(0, 0, 0, 0.05);
}

.panel-title {
  display: flex;
  justify-content: space-between;
  align-items: center;
  width: 100%;
  font-weight: 600;
}

.panel-title-actions {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.panel-title i {
  margin-right: 8px;
  color: #525252;
}

.panel-hint {
  color: var(--sub);
  font-size: 12px;
  font-family: "IBM Plex Mono", monospace;
}

.lookup-form {
  display: grid;
  gap: 10px;
}

.btn-row {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 8px;
}

.meta-line {
  margin-top: 10px;
  display: flex;
  justify-content: space-between;
  color: var(--sub);
  font-size: 12px;
  font-family: "IBM Plex Mono", monospace;
}

.lookup-state {
  margin-top: 10px;
  display: flex;
  gap: 8px;
  align-items: center;
  color: var(--sub);
  font-size: 12px;
}

.filter-stack {
  display: grid;
  gap: 10px;
}

.switch-row {
  border: 1px dashed var(--line-strong);
  border-radius: 10px;
  padding: 10px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  color: var(--sub);
}

.source-board {
  display: grid;
  grid-template-columns: repeat(2, minmax(250px, 1fr));
  gap: 10px;
}

.cluster-overview-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(120px, 1fr));
  gap: 10px;
}

.cluster-stat-cell {
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 10px;
  background: var(--surface-soft);
}

.cluster-stat-cell p {
  margin: 0;
  color: var(--sub);
  font-size: 12px;
}

.cluster-stat-cell strong {
  margin-top: 8px;
  display: block;
  font-size: 26px;
  line-height: 1;
  letter-spacing: -0.02em;
}

.cluster-worker-list {
  margin-top: 10px;
  display: grid;
  grid-template-columns: repeat(2, minmax(220px, 1fr));
  gap: 10px;
}

.cluster-worker-item {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 10px;
  background: #fff;
}

.cluster-worker-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}

.cluster-worker-item p {
  margin: 8px 0 0;
  color: var(--sub);
  font-size: 12px;
}

.cluster-worker-time {
  font-family: "IBM Plex Mono", monospace;
}

.source-cell {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 10px 12px;
  background: var(--surface-soft);
}

.source-name {
  margin: 0;
  font-weight: 600;
  color: #111;
}

.source-stats {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--sub);
}

.table-card :deep(.el-table th.el-table__cell) {
  background: #fafafa;
  color: #3f3f46;
  font-weight: 600;
}

.table-card :deep(.el-table td.el-table__cell),
.table-card :deep(.el-table th.el-table__cell) {
  border-bottom-color: var(--line);
  padding-top: 10px;
  padding-bottom: 10px;
}

.table-card :deep(.el-table) {
  border-radius: 12px;
  overflow: hidden;
}

.table-card :deep(.el-table__body tr:hover > td.el-table__cell) {
  background: #fcfcfc;
}

.replication-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.reason-tip-icon {
  color: #8b8b8b;
  font-size: 12px;
  cursor: help;
  transition: color 0.15s ease;
}

.reason-tip-icon:hover {
  color: #111;
}

.action-row {
  display: inline-flex;
  gap: 4px;
  flex-wrap: nowrap;
  white-space: nowrap;
}

.table-card :deep(.action-col .cell) {
  white-space: nowrap;
  overflow: visible;
}

.table-card :deep(.action-row .el-button + .el-button) {
  margin-left: 0;
}

.pager-wrap {
  margin-top: 12px;
  display: flex;
  justify-content: flex-end;
}

.batch-grid {
  margin-top: 12px;
}

.batch-preview-toolbar {
  margin-top: 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
}

.batch-preview-summary {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: var(--sub);
  font-size: 12px;
}

.batch-preview-count {
  font-family: "IBM Plex Mono", monospace;
}

.batch-preview-actions {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}

.batch-preview-table {
  margin-top: 10px;
}

.checkpoint {
  font-family: "IBM Plex Mono", monospace;
  color: var(--text);
}

.detail-stack {
  display: grid;
  gap: 12px;
}

.detail-panel {
  border: 1px solid var(--line);
  border-radius: 12px;
  background: #fff;
  padding: 12px;
}

.detail-panel h3 {
  margin: 0 0 12px;
  font-size: 14px;
  color: #3f3f46;
  display: flex;
  align-items: center;
  gap: 8px;
}

.detail-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}

.detail-item {
  border: 1px solid var(--line);
  border-radius: 10px;
  padding: 10px;
  background: #fff;
}

.detail-item span {
  display: block;
  color: #71717a;
  font-size: 12px;
}

.detail-item strong {
  margin-top: 6px;
  display: block;
  color: #18181b;
  word-break: break-word;
}

:deep(.el-card__header) {
  border-bottom-color: var(--line);
}

:deep(.el-card__body) {
  padding: 14px;
}

:deep(.el-input__wrapper),
:deep(.el-select__wrapper),
:deep(.el-input-number .el-input__wrapper) {
  border-radius: 10px;
  background: #fff;
  border: 1px solid var(--line);
  box-shadow: none;
}

:deep(.el-input__wrapper.is-focus),
:deep(.el-select__wrapper.is-focused),
:deep(.el-input-number .el-input__wrapper.is-focus) {
  box-shadow: 0 0 0 1px #111111 inset;
}

:deep(.el-button) {
  border-radius: 10px;
  font-weight: 500;
}

:deep(.el-button--primary) {
  background: #111;
  border-color: #111;
  color: #fff;
}

:deep(.el-button--primary:hover) {
  background: #242424;
  border-color: #242424;
}

:deep(.el-button:not(.el-button--primary):not(.el-button--success):not(.el-button--warning):not(.el-button--danger)) {
  border-color: var(--line);
  background: #fff;
  color: #2f2f2f;
}

:deep(.el-button:not(.el-button--primary):not(.el-button--success):not(.el-button--warning):not(.el-button--danger):hover) {
  border-color: #bdbdbd;
  color: #111;
}

.table-card :deep(.action-btn) {
  min-width: 46px;
  height: 28px;
  padding-left: 8px;
  padding-right: 8px;
  border-radius: 999px;
  font-weight: 600;
  font-size: 11px;
  letter-spacing: 0.01em;
}

.table-card :deep(.action-btn.el-button--success) {
  background: #f0fdf4;
  border-color: #bbf7d0;
  color: #166534;
}

.table-card :deep(.action-btn.el-button--warning) {
  background: #fffbeb;
  border-color: #fde68a;
  color: #92400e;
}

.table-card :deep(.action-btn.el-button--danger) {
  background: #fef2f2;
  border-color: #fecaca;
  color: #991b1b;
}

:deep(.el-tag) {
  border-radius: 999px;
  font-weight: 600;
}

:deep(.el-tag--success) {
  background: #f0fdf4;
  border-color: #bbf7d0;
  color: #166534;
}

:deep(.el-tag--warning) {
  background: #fffbeb;
  border-color: #fde68a;
  color: #92400e;
}

:deep(.el-tag--danger) {
  background: #fef2f2;
  border-color: #fecaca;
  color: #991b1b;
}

:deep(.el-tag--info) {
  background: #f4f4f5;
  border-color: #e4e4e7;
  color: #52525b;
}

@keyframes rise-fade {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

@media (max-width: 1380px) {
  .metric-grid {
    grid-template-columns: repeat(3, minmax(130px, 1fr));
  }
}

@media (max-width: 1120px) {
  .page-shell {
    padding: 14px;
  }

  h1 {
    font-size: 32px;
  }

  .hero {
    flex-direction: column;
    align-items: flex-start;
  }

  .workspace {
    grid-template-columns: 1fr;
  }

  .source-board {
    grid-template-columns: 1fr;
  }

  .cluster-overview-grid {
    grid-template-columns: 1fr;
  }

  .cluster-worker-list {
    grid-template-columns: 1fr;
  }

  .detail-grid {
    grid-template-columns: 1fr;
  }

  .action-row {
    gap: 4px;
  }
}
</style>
