<!--
input: dashboard/task API data, local filter state, auth-required browser event
output: operator-focused console UI with task list, detail drawer, forms, and settings
pos: single-page frontend entry for Binlog Server operations console
note: supports left-menu multi-view operations split while keeping create/edit/start/stop flows intact
-->
<template>
  <div class="page-shell" :class="{ 'page-shell--menu-collapsed': menuCollapsed }">
    <div class="orb orb-a" />
    <div class="orb orb-b" />

    <header class="hero">
      <div class="hero-copy">
        <p class="kicker"><i class="fa-solid fa-wave-square" /> BINLOG SERVER</p>
        <h1>Binlog Server 运维控制台</h1>
        <p class="hero-desc">
          优先发现异常、失败与延迟任务，并在详情中完成处置。
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
        <el-button @click="openSettings">
          <i class="fa-solid fa-gear" /> 设置
        </el-button>
      </div>
    </header>

    <section class="metric-grid">
      <article
        class="metric-card metric-card--danger"
        role="button"
        tabindex="0"
        data-testid="kpi-abnormal"
        :data-active="activeQuickFilter === 'abnormal'"
        @click="applyQuickFilter('abnormal')"
        @keydown.enter.prevent="applyQuickFilter('abnormal')"
        @keydown.space.prevent="applyQuickFilter('abnormal')"
      >
        <p><i class="fa-solid fa-triangle-exclamation" /> 异常任务</p>
        <strong data-testid="kpi-abnormal-value">{{ dashboard.summary.abnormal }}</strong>
      </article>
      <article
        class="metric-card metric-card--danger"
        role="button"
        tabindex="0"
        data-testid="kpi-failed"
        :data-active="activeQuickFilter === 'failed'"
        @click="applyQuickFilter('failed')"
        @keydown.enter.prevent="applyQuickFilter('failed')"
        @keydown.space.prevent="applyQuickFilter('failed')"
      >
        <p><i class="fa-solid fa-bug" /> 失败任务</p>
        <strong data-testid="kpi-failed-value">{{ dashboard.summary.failed }}</strong>
      </article>
      <article
        class="metric-card metric-card--warning"
        role="button"
        tabindex="0"
        data-testid="kpi-delayed"
        :data-active="activeQuickFilter === 'delayed'"
        @click="applyQuickFilter('delayed')"
        @keydown.enter.prevent="applyQuickFilter('delayed')"
        @keydown.space.prevent="applyQuickFilter('delayed')"
      >
        <p><i class="fa-solid fa-hourglass-half" /> 延迟任务</p>
        <strong data-testid="kpi-delayed-value">{{ dashboard.summary.delayed }}</strong>
      </article>
      <article
        class="metric-card"
        role="button"
        tabindex="0"
        data-testid="kpi-running"
        :data-active="activeQuickFilter === 'running'"
        @click="applyQuickFilter('running')"
        @keydown.enter.prevent="applyQuickFilter('running')"
        @keydown.space.prevent="applyQuickFilter('running')"
      >
        <p><i class="fa-solid fa-play" /> 运行中任务</p>
        <strong>{{ dashboard.summary.running }}</strong>
      </article>
      <article
        class="metric-card"
        role="button"
        tabindex="0"
        data-testid="kpi-all"
        :data-active="activeQuickFilter === 'all'"
        @click="applyQuickFilter('all')"
        @keydown.enter.prevent="applyQuickFilter('all')"
        @keydown.space.prevent="applyQuickFilter('all')"
      >
        <p><i class="fa-solid fa-layer-group" /> 总任务</p>
        <strong>{{ dashboard.summary.total }}</strong>
      </article>
      <article
        class="metric-card metric-card--success"
        role="button"
        tabindex="0"
        data-testid="kpi-normal"
        :data-active="activeQuickFilter === 'normal'"
        @click="applyQuickFilter('normal')"
        @keydown.enter.prevent="applyQuickFilter('normal')"
        @keydown.space.prevent="applyQuickFilter('normal')"
      >
        <p><i class="fa-solid fa-circle-check" /> 正常任务</p>
        <strong>{{ dashboard.summary.normal }}</strong>
      </article>
    </section>

    <el-alert
      v-if="authRequiredMessage"
      data-testid="auth-required-banner"
      class="auth-required-banner"
      type="warning"
      show-icon
      :closable="false"
      :title="authRequiredTitle"
      :description="authRequiredMessage"
    />

    <section class="workspace" :class="{ 'workspace--no-pane': activeView === 'overview' || activeView === 'workers' }">
      <aside class="nav-pane" :class="{ 'nav-pane--collapsed': menuCollapsed }">
        <div class="nav-head">
          <div class="nav-brand" :title="menuCollapsed ? 'Binlog Console' : ''">
            <span class="nav-brand-icon"><i class="fa-solid fa-wave-square" /></span>
            <span class="nav-brand-text">Binlog Console</span>
          </div>
        </div>
        <button
          class="nav-item"
          :class="{ 'nav-item--active': activeView === 'overview' }"
          data-testid="view-nav-overview"
          @click="switchView('overview')"
        >
          <span><i class="fa-solid fa-border-all" /><span class="nav-label">总览</span></span>
          <span class="nav-badge">{{ dashboard.summary.total }}</span>
        </button>
        <button
          class="nav-item"
          :class="{ 'nav-item--active': activeView === 'tasks' }"
          data-testid="view-nav-tasks"
          @click="switchView('tasks')"
        >
          <span><i class="fa-solid fa-table-list" /><span class="nav-label">任务列表</span></span>
          <span class="nav-badge">{{ filteredTasks.length }}</span>
        </button>
        <button
          class="nav-item"
          :class="{ 'nav-item--active': activeView === 'sources' }"
          data-testid="view-nav-sources"
          @click="switchView('sources')"
        >
          <span><i class="fa-solid fa-database" /><span class="nav-label">源库覆盖</span></span>
          <span class="nav-badge">{{ dashboard.sources.length }}</span>
        </button>
        <button
          class="nav-item"
          :class="{ 'nav-item--active': activeView === 'workers' }"
          data-testid="view-nav-workers"
          @click="switchView('workers')"
        >
          <span><i class="fa-solid fa-network-wired" /><span class="nav-label">Worker 运维</span></span>
          <span class="nav-badge">{{ cluster.overview.worker_count }}</span>
        </button>
        <button
          class="nav-item"
          :class="{ 'nav-item--active': activeView === 'alerts' }"
          data-testid="view-nav-alerts"
          @click="switchView('alerts')"
        >
          <span><i class="fa-solid fa-bell" /><span class="nav-label">异常与告警</span></span>
          <span class="nav-badge">{{ dashboard.summary.abnormal + dashboard.summary.failed + dashboard.summary.delayed }}</span>
        </button>
        <div class="nav-foot">
          <button
            class="nav-collapse-btn nav-collapse-btn--dock"
            :title="menuCollapsed ? '展开菜单' : '折叠菜单'"
            data-testid="view-nav-collapse"
            @click="menuCollapsed = !menuCollapsed"
          >
            <i :class="menuCollapsed ? 'fa-solid fa-angles-right' : 'fa-solid fa-angles-left'" />
          </button>
        </div>
      </aside>

      <aside v-if="activeView === 'tasks' || activeView === 'alerts' || activeView === 'sources'" class="left-pane">
        <el-card v-if="activeView === 'tasks' || activeView === 'alerts'" shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-sliders" /> 运维筛选</span>
              <span class="panel-hint">优先排查异常</span>
            </div>
          </template>

          <div class="filter-stack">
            <div class="filter-chip-row">
              <el-button size="small" :type="uiFilter.onlyAlert ? 'primary' : 'default'" @click="applyQuickFilter('alert')">仅看告警</el-button>
              <el-button size="small" @click="applyQuickFilter('abnormal')">异常</el-button>
              <el-button size="small" @click="applyQuickFilter('failed')">失败</el-button>
              <el-button size="small" @click="applyQuickFilter('delayed')">延迟</el-button>
            </div>
            <el-input v-model="uiFilter.keyword" clearable placeholder="按任务 ID/名称搜索" />
            <el-select v-model="uiFilter.taskState">
              <el-option label="全部任务状态" value="ALL" />
              <el-option v-for="state in taskStates" :key="state" :label="stateLabel(state)" :value="state" />
            </el-select>
            <el-select v-model="uiFilter.replicationStatus">
              <el-option label="全部复制状态" value="ALL" />
              <el-option v-for="status in replicationStatuses" :key="status" :label="replicationStatusLabel(status)" :value="status" />
            </el-select>
            <el-input v-model="uiFilter.sourceKeyword" clearable placeholder="按源库 host:port 过滤" />
            <el-select v-model="uiFilter.sortBy">
              <el-option label="风险优先" value="delay_desc" />
              <el-option label="最近更新优先" value="updated_desc" />
              <el-option label="任务名 A-Z" value="name_asc" />
            </el-select>
            <div class="switch-row">
              <span>告警任务优先显示</span>
              <el-switch v-model="uiFilter.onlyAlert" />
            </div>
            <div class="filter-summary" data-testid="filter-summary">当前显示 {{ filteredTasks.length }} 个任务</div>
            <el-button @click="resetUiFilter">重置筛选</el-button>
          </div>
        </el-card>

        <el-card v-if="activeView === 'sources'" shadow="never" class="panel-card panel-card--secondary">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-magnifying-glass" /> 源库反查</span>
              <span class="panel-hint">辅助定位</span>
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
            <span>延迟阈值 {{ dashboard.threshold_seconds }}s</span>
            <span>{{ formatTs(dashboard.generated_at) }}</span>
          </div>

          <div v-if="lookup.checked" class="lookup-state">
            <el-tag :type="lookup.exists ? 'success' : 'info'">
              {{ lookup.exists ? "已存在采集任务" : "未找到采集任务" }}
            </el-tag>
            <span>匹配数量：{{ lookup.count }}</span>
          </div>
        </el-card>
      </aside>

      <section class="right-pane">
        <el-card
          v-if="activeView === 'overview' || activeView === 'workers'"
          shadow="never"
          class="panel-card"
        >
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-sitemap" /> 集群视图</span>
              <span class="panel-hint">{{ cluster.overview.worker_count }} 个 Worker</span>
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

          <div v-if="activeView === 'workers'" class="cluster-worker-list">
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
              data-testid="workers-empty"
              description="暂无 worker 数据"
              :image-size="56"
            />
          </div>
          <div v-else class="overview-note">
            Worker 明细已迁移到「Worker 运维」工作区，当前仅保留集群摘要。
          </div>
        </el-card>

        <el-card
          v-if="activeView === 'overview' || activeView === 'sources'"
          shadow="never"
          class="panel-card"
        >
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-network-wired" /> 源库覆盖</span>
              <span class="panel-hint">{{ dashboard.sources.length }} hosts</span>
            </div>
          </template>

          <div class="source-board" v-if="dashboard.sources.length > 0">
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
          <el-empty
            v-else
            data-testid="sources-empty"
            description="暂无 source coverage 数据"
            :image-size="56"
          />
        </el-card>

        <el-card
          v-if="activeView === 'tasks' || activeView === 'alerts'"
          shadow="never"
          class="panel-card table-card"
        >
          <template #header>
            <div class="panel-title">
              <span>
                <i :class="activeView === 'alerts' ? 'fa-solid fa-bell' : 'fa-solid fa-table'" />
                {{ activeView === "alerts" ? "异常与告警任务" : "任务列表" }}
              </span>
              <div class="panel-title-actions">
                <span class="panel-hint">
                  {{
                    activeView === "alerts"
                      ? `告警筛选 ${filteredTasks.length} / 总计 ${dashboard.tasks.length}`
                      : `筛选后 ${filteredTasks.length} / 总计 ${dashboard.tasks.length}`
                  }}
                </span>
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
            :row-class-name="taskRowClassName"
            @row-click="onRowClick"
          >
            <el-table-column label="ID" width="70">
              <template #default="{ row }"><span :data-testid="`task-row-${row.task.id}`">{{ row.task.id }}</span></template>
            </el-table-column>
            <el-table-column label="名称" min-width="180">
              <template #default="{ row }">{{ row.task.name }}</template>
            </el-table-column>
            <el-table-column label="任务状态" width="140">
              <template #default="{ row }">
                <el-tag size="small" :type="stateTagType(row.task.state)">{{ stateLabel(row.task.state) }}</el-tag>
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
            <el-table-column label="复制状态" width="170">
              <template #default="{ row }">
                <div class="replication-cell">
                  <el-tag size="small" :type="replicationTagType(row.replication.status)">
                    {{ replicationStatusLabel(row.replication.status) }}
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
            <el-table-column label="操作" width="110" fixed="right" class-name="action-col">
              <template #default="{ row }">
                <div class="action-row">
                  <el-button class="action-btn" size="small" :data-testid="`task-detail-trigger-${row.task.id}`" @click.stop="showDetail(row.task)">详情</el-button>
                </div>
              </template>
            </el-table-column>
          </el-table>
          <el-empty
            v-if="pagedTasks.length === 0"
            data-testid="task-table-empty"
            description="暂无任务"
            :image-size="56"
          />

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

    <el-drawer v-model="detailVisible" data-testid="task-drawer" size="66%" :title="detailTask ? `任务详情 #${detailTask.id}` : '任务详情'">
      <template v-if="detailTask">
        <div class="detail-stack">
          <section class="detail-panel detail-panel--hero">
            <div class="detail-hero">
              <div>
                <div class="detail-hero-kicker">任务处理中心</div>
                <h3><i class="fa-solid fa-circle-info" /> {{ detailTask.name }}</h3>
                <div class="detail-hero-meta">
                  <el-tag data-testid="task-drawer-status" :type="stateTagType(detailTask.state)">{{ stateLabel(detailTask.state) }}</el-tag>
                  <el-tag v-if="detailReplication" data-testid="task-drawer-replication" :type="replicationTagType(detailReplication.status)">{{ replicationStatusLabel(detailReplication.status) }}</el-tag>
                  <span data-testid="task-drawer-source">源库 {{ sourceLabel(detailTask) }}</span>
                  <span>Cluster Key {{ detailTask.cluster_key || "--" }}</span>
                </div>
              </div>
              <div class="detail-action-row" data-testid="task-drawer-actions">
                <el-button data-testid="task-action-edit" @click="openEdit(detailTask)">编辑</el-button>
                <el-button data-testid="task-action-start" type="success" @click="onStart(detailTask)">启动</el-button>
                <el-button data-testid="task-action-stop" type="warning" @click="onStop(detailTask)">停止</el-button>
                <el-button data-testid="task-action-delete" type="danger" plain @click="onDelete(detailTask)">删除</el-button>
              </div>
            </div>
            <div class="detail-grid detail-grid--summary">
              <div class="detail-item"><span>当前状态</span><strong>{{ stateLabel(detailTask.state) }}</strong></div>
              <div class="detail-item"><span>复制状态</span><strong>{{ detailReplication ? replicationStatusLabel(detailReplication.status) : "--" }}</strong></div>
              <div class="detail-item"><span>延迟</span><strong>{{ detailReplication ? `${formatDelay(detailReplication.delay_seconds, detailReplication.has_progress)} 秒` : "--" }}</strong></div>
              <div class="detail-item"><span>Lease 状态</span><strong>{{ leaseRiskLabel(detailTask, detailLease) }}</strong></div>
            </div>
          </section>

          <section class="detail-panel" v-if="detailReplication">
            <h3><i class="fa-solid fa-wave-square" /> 复制与位点</h3>
            <div class="detail-grid">
              <div class="detail-item">
                <span>复制状态</span>
                <strong><el-tag :type="replicationTagType(detailReplication.status)">{{ replicationStatusLabel(detailReplication.status) }}</el-tag></strong>
              </div>
              <div class="detail-item"><span>延迟</span><strong>{{ formatDelay(detailReplication.delay_seconds, detailReplication.has_progress) }} 秒</strong></div>
              <div class="detail-item"><span>当前 Checkpoint</span><strong data-testid="task-drawer-checkpoint">{{ formatCheckpoint(checkpoint) }}</strong></div>
              <div class="detail-item"><span>异常原因</span><strong>{{ formatReplicationReason(detailReplication) }}</strong></div>
              <div class="detail-item"><span>告警阈值</span><strong>{{ detailReplication.threshold_seconds || "--" }} 秒</strong></div>
              <div class="detail-item"><span>最近事件时间</span><strong>{{ formatTs(detailReplication.last_event_at) }}</strong></div>
              <div class="detail-item"><span>最近位点</span><strong>{{ detailReplication.last_event_file || "-" }}:{{ detailReplication.last_event_pos || 0 }}</strong></div>
              <div class="detail-item"><span>状态码</span><strong>{{ detailReplication.reason || "--" }}</strong></div>
            </div>
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-circle-info" /> 基础信息</h3>
            <div class="detail-grid">
              <div class="detail-item"><span>名称</span><strong>{{ detailTask.name }}</strong></div>
              <div class="detail-item"><span>Cluster Key</span><strong>{{ detailTask.cluster_key || "--" }}</strong></div>
              <div class="detail-item"><span>源库</span><strong>{{ sourceLabel(detailTask) }}</strong></div>
              <div class="detail-item"><span>起点模式</span><strong>{{ detailTask.start?.mode || "--" }}</strong></div>
              <div class="detail-item"><span>SemiSync</span><strong>{{ detailTask.source?.semi_sync ? "开启" : "关闭" }}</strong></div>
              <div class="detail-item"><span>保留天数</span><strong>{{ detailTask.storage?.retention_days || "--" }}</strong></div>
            </div>
          </section>

          <section class="detail-panel" v-if="detailLease">
            <h3><i class="fa-solid fa-key" /> Lease 与 Worker</h3>
            <div class="detail-grid">
              <div class="detail-item"><span>归属 Worker</span><strong data-testid="task-drawer-worker">{{ detailLease.owner_worker_id || "--" }}</strong></div>
              <div class="detail-item"><span>Epoch</span><strong>{{ detailLease.epoch || "--" }}</strong></div>
              <div class="detail-item"><span>Lease 状态</span><strong>{{ leaseRiskLabel(detailTask, detailLease) }}</strong></div>
              <div class="detail-item"><span>更新时间</span><strong>{{ formatTs(detailLease.updated_at) }}</strong></div>
            </div>
          </section>

          <section class="detail-panel">
            <h3><i class="fa-solid fa-file-lines" /> 文件与上传</h3>
            <div class="detail-panel-toolbar">
              <el-button
                v-if="files.some((file) => file.upload_state === 'UPLOAD_FAILED')"
                data-testid="retry-upload-action"
                size="small"
                type="warning"
                @click="retryFailedUploads(detailTask)"
              >
                重试上传失败文件
              </el-button>
            </div>
            <el-table :data="files" size="small" border>
              <el-table-column prop="file_name" label="文件" min-width="180" />
              <el-table-column prop="size_bytes" label="大小" width="100" />
              <el-table-column prop="start_pos" label="起始位点" width="100" />
              <el-table-column prop="end_pos" label="结束位点" width="100" />
              <el-table-column prop="upload_state" label="上传状态" width="130">
                <template #default="{ row }">
                  <span :data-testid="`file-upload-state-${row.file_name}`">{{ row.upload_state }}</span>
                </template>
              </el-table-column>
              <el-table-column prop="object_key" label="对象键" min-width="190" />
            </el-table>
          </section>

          <section class="detail-panel" data-testid="task-drawer-runs">
            <h3><i class="fa-solid fa-clock-rotate-left" /> 运行历史（最近 {{ runHistoryLimit }} 条）</h3>
            <el-table :data="detailRunsLimited" size="small" border>
              <el-table-column prop="run_id" label="运行 ID" min-width="180" />
              <el-table-column prop="worker_id" label="Worker" min-width="120" />
              <el-table-column prop="epoch" label="Epoch" width="100" />
              <el-table-column label="开始时间" min-width="170">
                <template #default="{ row }">{{ formatTs(row.started_at) }}</template>
              </el-table-column>
              <el-table-column label="结束时间" min-width="170">
                <template #default="{ row }">{{ formatTs(row.ended_at) }}</template>
              </el-table-column>
              <el-table-column label="结束原因" min-width="140">
                <template #default="{ row }">{{ row.end_reason || "--" }}</template>
              </el-table-column>
            </el-table>
            <el-empty v-if="detailRunsLimited.length === 0" description="暂无运行历史" :image-size="56" />
          </section>

          <section class="detail-panel" data-testid="task-drawer-events">
            <h3><i class="fa-solid fa-clock-rotate-left" /> 事件时间线</h3>
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

    <!-- Settings Dialog -->
    <el-dialog v-model="settingsVisible" data-testid="settings-dialog" title="设置" width="480px">
      <el-form label-width="100px">
        <el-form-item label="API Token">
          <el-input
            v-model="settingsToken"
            data-testid="settings-token-input"
            type="password"
            show-password
            placeholder="输入 Bearer Token"
          />
          <div class="form-hint">
            配置 API 认证令牌。启用认证后需要设置此项才能访问 API。
          </div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="settingsVisible = false">取消</el-button>
        <el-button type="primary" @click="saveSettings">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, reactive, ref, watch, onMounted, onBeforeUnmount } from "vue";
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
  retryUpload,
  startTask,
  stopTask,
  updateTask,
} from "./api";
import { getAuthToken, setAuthToken } from "./utils/auth.js";

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
const VIEW_HASH_MAP = {
  overview: "#/overview",
  tasks: "#/tasks",
  sources: "#/sources",
  workers: "#/workers",
  alerts: "#/alerts",
};

// Settings state
const settingsVisible = ref(false);
const settingsToken = ref("");
const authRequiredTitle = "需要重新认证";
const authRequiredMessage = ref("");
const activeQuickFilter = ref("all");
const activeView = ref(resolveViewFromHash());
const menuCollapsed = ref(false);

// Auth + URL route listeners
function handleAuthRequired(event) {
  authRequiredMessage.value = event?.detail?.message || "接口认证已失效或尚未配置。";
  settingsVisible.value = true;
  settingsToken.value = getAuthToken() || "";
}

function handleHashChange() {
  const { view, mode } = parseHashRoute();
  if (view === "tasks" && mode === "alerts") {
    activateAlertsView(false);
    return;
  }
  if (view === "alerts") {
    activateAlertsView(false);
    return;
  }
  activeView.value = view;
}

onMounted(() => {
  window.addEventListener("auth-required", handleAuthRequired);
  window.addEventListener("hashchange", handleHashChange);
  handleHashChange();
  syncHash(activeView.value, true);
});

onBeforeUnmount(() => {
  window.removeEventListener("auth-required", handleAuthRequired);
  window.removeEventListener("hashchange", handleHashChange);
});

function openSettings() {
  settingsToken.value = getAuthToken() || "";
  settingsVisible.value = true;
}

function saveSettings() {
  setAuthToken(settingsToken.value);
  settingsVisible.value = false;
  authRequiredMessage.value = "";
  window.__authDialogShown = false;
  ElMessage.success("设置已保存");
  refreshAll();
}

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
const TASK_STATE_LABELS = {
  CREATED: "已创建",
  STARTING: "启动中",
  RUNNING: "运行中",
  RETRY_BACKOFF: "重试退避",
  STOPPING: "停止中",
  STOPPED: "已停止",
  FAILED: "失败",
};
const REPLICATION_STATUS_LABELS = {
  NORMAL: "正常",
  DELAYED: "延迟",
  ABNORMAL: "异常",
  IDLE: "空闲",
};

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
      return updatedMs > 0 && nowRefMs() - updatedMs <= LEASE_RISK_SECONDS * 1000;
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
  activeQuickFilter.value = "all";
}

function resolveViewFromHash() {
  return parseHashRoute().view;
}

function parseHashRoute() {
  const hash = typeof window !== "undefined" ? String(window.location.hash || "") : "";
  const normalized = hash.replace(/^#/, "");
  const [path, queryText = ""] = normalized.split("?");
  const query = new URLSearchParams(queryText || "");
  const mode = query.get("mode") || "";
  if (path === "/tasks") return { view: "tasks", mode };
  if (path === "/sources") return { view: "sources", mode };
  if (path === "/workers") return { view: "workers", mode };
  if (path === "/alerts") return { view: "alerts", mode };
  return { view: "overview", mode };
}

function syncHash(view, replace = false) {
  if (typeof window === "undefined") return;
  const nextHash = VIEW_HASH_MAP[view] || VIEW_HASH_MAP.overview;
  if (window.location.hash === nextHash) return;
  if (replace) {
    window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}${nextHash}`);
    return;
  }
  window.location.hash = nextHash;
}

function activateAlertsView(syncHashWhenNeeded = true) {
  activeQuickFilter.value = "alert";
  uiFilter.onlyAlert = true;
  uiFilter.taskState = "ALL";
  uiFilter.replicationStatus = "ALL";
  activeView.value = "alerts";
  if (syncHashWhenNeeded) syncHash("alerts");
}

function switchView(view) {
  if (view === "alerts") {
    activateAlertsView(true);
    return;
  }
  activeView.value = view;
  syncHash(view);
}

function applyQuickFilter(kind, options = {}) {
  const { syncHashWhenNeeded = true } = options;
  activeQuickFilter.value = kind;
  if (kind === "all") {
    resetUiFilter();
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }

  uiFilter.onlyAlert = false;
  uiFilter.taskState = "ALL";
  uiFilter.replicationStatus = "ALL";

  if (kind === "alert") {
    activateAlertsView(syncHashWhenNeeded);
    return;
  }
  if (kind === "abnormal") {
    uiFilter.replicationStatus = "ABNORMAL";
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }
  if (kind === "failed") {
    uiFilter.taskState = "FAILED";
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }
  if (kind === "delayed") {
    uiFilter.replicationStatus = "DELAYED";
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }
  if (kind === "running") {
    uiFilter.taskState = "RUNNING";
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }
  if (kind === "normal") {
    uiFilter.replicationStatus = "NORMAL";
    activeView.value = "tasks";
    if (syncHashWhenNeeded) syncHash("tasks");
    return;
  }
  activeView.value = "tasks";
  if (syncHashWhenNeeded) syncHash("tasks");
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

function taskRowClassName({ row }) {
  return row?.task?.id ? `task-row task-row-${row.task.id}` : "task-row";
}

function onRowClick(row) {
  void showDetail(row.task);
}

async function retryFailedUploads(task) {
  try {
    await retryUpload(task.id, 100);
    files.value = await listFiles(task.id, 80);
    ElMessage.success("失败文件已触发重试上传");
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
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
  const stale = nowRefMs() - updatedMs > LEASE_RISK_SECONDS * 1000;
  return stale ? "风险" : "正常";
}

function stateLabel(state) {
  return TASK_STATE_LABELS[state] || state || "--";
}

function replicationStatusLabel(status) {
  return REPLICATION_STATUS_LABELS[status] || status || "--";
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

function nowRefMs() {
  const dashboardMs = toTimeMs(dashboard.generated_at);
  return dashboardMs > 0 ? dashboardMs : Date.now();
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
  padding: 24px 24px 24px 252px;
  font-family: "Geist", "SF Pro Display", "PingFang SC", sans-serif;
  color: var(--text);
  background:
    radial-gradient(circle at 0% 0%, #ffffff 0, #fafafa 42%),
    linear-gradient(90deg, rgba(0, 0, 0, 0.015) 1px, transparent 1px),
    linear-gradient(rgba(0, 0, 0, 0.015) 1px, transparent 1px);
  background-size: auto, 28px 28px, 28px 28px;
}

.page-shell--menu-collapsed {
  padding-left: 96px;
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
  max-width: 560px;
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
  cursor: pointer;
  transition:
    transform 0.2s ease,
    border-color 0.2s ease,
    box-shadow 0.2s ease;
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

.metric-card:hover {
  transform: translateY(-1px);
  border-color: var(--line-strong);
  box-shadow: 0 6px 16px rgba(0, 0, 0, 0.06);
}

.metric-card--danger {
  border-color: #fecaca;
  background: linear-gradient(180deg, #fff8f8 0%, #fff 100%);
}

.metric-card--danger p,
.metric-card--danger i,
.metric-card--danger strong {
  color: #991b1b;
}

.metric-card--warning {
  border-color: #fde68a;
  background: linear-gradient(180deg, #fffdf2 0%, #fff 100%);
}

.metric-card--warning p,
.metric-card--warning i,
.metric-card--warning strong {
  color: #92400e;
}

.metric-card--success {
  border-color: #bbf7d0;
}

.workspace {
  display: grid;
  grid-template-columns: 320px minmax(0, 1fr);
  gap: 12px;
}

.workspace--no-pane {
  grid-template-columns: 1fr;
}

.nav-pane {
  position: fixed;
  left: 0;
  top: 0;
  bottom: 0;
  width: 228px;
  overflow: auto;
  border-right: 1px solid #e2e8f0;
  border-radius: 0;
  background: #f7f8fa;
  padding: 10px 6px 8px;
  display: flex;
  flex-direction: column;
  gap: 3px;
}

.nav-pane--collapsed {
  width: 72px;
  align-items: center;
}

.nav-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 4px;
  margin-bottom: 2px;
}

.nav-foot {
  margin-top: auto;
  padding-top: 8px;
}

.nav-pane--collapsed .nav-head {
  justify-content: center;
}

.nav-brand {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: #0f172a;
  min-height: 30px;
}

.nav-brand-icon {
  width: 24px;
  height: 24px;
  border-radius: 6px;
  background: #e2e8f0;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: #334155;
  font-size: 12px;
}

.nav-brand-text {
  font-size: 13px;
  font-weight: 600;
  letter-spacing: 0.01em;
}

.nav-pane--collapsed .nav-brand-text {
  display: none;
}

.nav-collapse-btn {
  border: 0;
  background: transparent;
  color: #475569;
  border-radius: 8px;
  width: 30px;
  height: 30px;
  cursor: pointer;
}

.nav-collapse-btn--dock {
  width: 100%;
  height: 34px;
  text-align: left;
  padding-left: 8px;
}

.nav-pane--collapsed .nav-collapse-btn--dock {
  width: 38px;
  padding-left: 0;
  text-align: center;
}

.nav-pane--collapsed .nav-foot {
  width: 38px;
}

.nav-item {
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #0f172a;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
  padding: 6px 8px;
  font-size: 14px;
  font-weight: 500;
  cursor: pointer;
  transition:
    background 0.15s ease,
    color 0.15s ease;
}

.nav-item span {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}

.nav-item i {
  color: #64748b;
}

.nav-item--active {
  background: #e9edf3;
  color: #0b1220;
  font-weight: 600;
}

.nav-item:hover {
  background: #edf1f6;
}

.nav-pane--collapsed .nav-item {
  width: 38px;
  height: 38px;
  justify-content: center;
  padding: 0;
}

.nav-pane--collapsed .nav-item span {
  width: 100%;
  justify-content: center;
}

.nav-pane--collapsed .nav-label,
.nav-pane--collapsed .nav-badge {
  display: none;
}

.nav-badge {
  font-family: "IBM Plex Mono", monospace;
  color: #94a3b8;
  font-size: 11px;
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

.panel-card--secondary {
  opacity: 0.98;
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

.form-hint {
  margin-top: 6px;
  color: var(--sub);
  font-size: 12px;
  line-height: 1.5;
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

.filter-chip-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.filter-summary {
  color: var(--sub);
  font-size: 12px;
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

.overview-note {
  margin-top: 10px;
  border: 1px dashed var(--line-strong);
  border-radius: 10px;
  padding: 10px;
  color: var(--sub);
  font-size: 12px;
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

.table-card :deep(.el-table__body tr) {
  cursor: pointer;
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

.detail-panel--hero {
  border-color: var(--line-strong);
  background: linear-gradient(180deg, #fcfcfc 0%, #fff 100%);
}

.detail-hero {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}

.detail-hero-kicker {
  color: var(--sub);
  font-size: 12px;
  margin-bottom: 6px;
}

.detail-hero-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  margin-top: 10px;
  color: var(--sub);
  font-size: 12px;
}

.detail-action-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: flex-start;
}

.detail-grid--summary {
  margin-top: 12px;
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
  min-width: 56px;
  height: 28px;
  padding-left: 10px;
  padding-right: 10px;
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

  .page-shell--menu-collapsed {
    padding-left: 14px;
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

  .nav-pane {
    position: static;
    top: auto;
    width: auto;
    max-height: none;
    overflow: visible;
    border-right: 1px solid var(--line);
    border-radius: 12px;
    background: #f8fafc;
    display: flex;
    flex-direction: row;
    flex-wrap: wrap;
    gap: 8px;
  }

  .nav-pane--collapsed {
    width: auto;
  }

  .nav-pane--collapsed .nav-brand-text,
  .nav-pane--collapsed .nav-label,
  .nav-pane--collapsed .nav-badge {
    display: inline-flex;
  }

  .nav-pane--collapsed .nav-item {
    width: auto;
    height: auto;
    justify-content: space-between;
    padding: 6px 8px;
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

  .detail-hero {
    flex-direction: column;
  }
}
</style>
