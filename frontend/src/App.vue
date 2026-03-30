<!--
input: dashboard/task API data, local filter state, auth-required browser event
output: operator-focused console UI with task list, detail drawer, forms, and settings
pos: single-page frontend entry for Binlog Server operations console
note: supports left-menu multi-view operations split while keeping create/edit/start/stop flows intact
-->
<template>
  <el-config-provider :locale="elLocale">
  <div class="page-shell" :class="{ 'page-shell--menu-collapsed': menuCollapsed }">
    <div class="orb orb-a" />
    <div class="orb orb-b" />

    <AppHeader
      :loading="loading"
      @create="openCreate"
      @batch-create="openBatchCreate"
      @refresh="refreshAll"
      @settings="openSettings"
    />

    <MetricGrid
      :summary="dashboard.summary"
      :active-quick-filter="activeQuickFilter"
      @filter="applyQuickFilter"
    />

    <AlertBanner
      v-if="authRequiredMessage"
      :title="authRequiredTitle"
      :message="authRequiredMessage"
    />

    <section class="workspace" :class="{ 'workspace--no-pane': activeView === 'overview' || activeView === 'workers' }">
      <NavPane
        v-model="menuCollapsed"
        :active-view="activeView"
        :total-count="dashboard.summary.total"
        :filtered-tasks-count="filteredTasks.length"
        :sources-count="dashboard.sources.length"
        :worker-count="cluster.overview.worker_count"
        :alert-count="dashboard.summary.abnormal + dashboard.summary.failed + dashboard.summary.delayed"
        @switch-view="switchView"
      />

      <aside v-if="activeView === 'tasks' || activeView === 'alerts' || activeView === 'sources'" class="left-pane">
        <el-card v-if="activeView === 'tasks' || activeView === 'alerts'" shadow="never" class="panel-card">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-sliders" /> {{ $t('filter.title') }}</span>
              <span class="panel-hint">{{ $t('filter.hint') }}</span>
            </div>
          </template>

          <div class="filter-stack">
            <div class="filter-chip-row">
              <el-button size="small" :type="uiFilter.onlyAlert ? 'primary' : 'default'" @click="applyQuickFilter('alert')">{{ $t('btn.alertOnly') }}</el-button>
              <el-button size="small" @click="applyQuickFilter('abnormal')">{{ $t('metrics.abnormal') }}</el-button>
              <el-button size="small" @click="applyQuickFilter('failed')">{{ $t('metrics.failed') }}</el-button>
              <el-button size="small" @click="applyQuickFilter('delayed')">{{ $t('metrics.delayed') }}</el-button>
            </div>
            <el-input v-model="uiFilter.keyword" clearable :placeholder="$t('placeholder.searchTask')" />
            <el-select v-model="uiFilter.taskState">
              <el-option :label="$t('filter.allTaskStates')" value="ALL" />
              <el-option v-for="state in taskStates" :key="state" :label="stateLabel(state)" :value="state" />
            </el-select>
            <el-select v-model="uiFilter.replicationStatus">
              <el-option :label="$t('filter.allReplicationStatuses')" value="ALL" />
              <el-option v-for="status in replicationStatuses" :key="status" :label="replicationStatusLabel(status)" :value="status" />
            </el-select>
            <el-input v-model="uiFilter.sourceKeyword" clearable :placeholder="$t('placeholder.filterSource')" />
            <el-select v-model="uiFilter.sortBy">
              <el-option :label="$t('filter.sortByDelay')" value="delay_desc" />
              <el-option :label="$t('filter.sortByUpdated')" value="updated_desc" />
              <el-option :label="$t('filter.sortByName')" value="name_asc" />
            </el-select>
            <div class="switch-row">
              <span>{{ $t('filter.alertFirst') }}</span>
              <el-switch v-model="uiFilter.onlyAlert" />
            </div>
            <div class="filter-summary" data-testid="filter-summary" aria-live="polite" aria-atomic="true">{{ $t('filter.showingCount', { count: filteredTasks.length }) }}</div>
            <el-button @click="resetUiFilter">{{ $t('btn.resetFilter') }}</el-button>
          </div>
        </el-card>

        <el-card v-if="activeView === 'sources'" shadow="never" class="panel-card panel-card--secondary">
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-magnifying-glass" /> {{ $t('source.lookupTitle') }}</span>
              <span class="panel-hint">{{ $t('source.lookupHint') }}</span>
            </div>
          </template>

          <div class="lookup-form">
            <el-input
              v-model="sourceQuery.host"
              :placeholder="$t('placeholder.hostExample')"
              clearable
            />
            <el-input-number
              v-model="sourceQuery.port"
              :min="1"
              :max="65535"
              controls-position="right"
            />
            <div class="btn-row">
              <el-button type="primary" :loading="loading" @click="applySourceFilter">{{ $t('btn.query') }}</el-button>
              <el-button @click="clearSourceFilter">{{ $t('btn.clear') }}</el-button>
            </div>
          </div>

          <div class="meta-line">
            <span>{{ $t('source.delayThreshold', { seconds: dashboard.threshold_seconds }) }}</span>
            <span>{{ formatTs(dashboard.generated_at) }}</span>
          </div>

          <div v-if="lookup.checked" class="lookup-state">
            <el-tag :type="lookup.exists ? 'success' : 'info'">
              {{ lookup.exists ? $t('source.taskExists') : $t('source.taskNotFound') }}
            </el-tag>
            <span>{{ $t('source.matchCount', { count: lookup.count }) }}</span>
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
              <span><i class="fa-solid fa-sitemap" /> {{ $t('cluster.title') }}</span>
              <span class="panel-hint">{{ $t('cluster.workerCount', { count: cluster.overview.worker_count }) }}</span>
            </div>
          </template>

          <div class="cluster-overview-grid">
            <div class="cluster-stat-cell">
              <p>{{ $t('cluster.taskCount') }}</p>
              <strong class="cluster-stat-value">{{ cluster.overview.task_count }}</strong>
            </div>
            <div class="cluster-stat-cell">
              <p>{{ $t('cluster.runningTaskCount') }}</p>
              <strong class="cluster-stat-value">{{ cluster.overview.running_task_count }}</strong>
            </div>
            <div class="cluster-stat-cell">
              <p>{{ $t('cluster.leasedTaskCount') }}</p>
              <strong class="cluster-stat-value">{{ cluster.overview.leased_task_count }}</strong>
            </div>
          </div>

          <div v-if="activeView === 'workers'" class="cluster-worker-list">
            <div
              v-for="worker in workerRows"
              :key="worker.worker_id"
              class="cluster-worker-item"
            >
              <div class="cluster-worker-head">
                <strong class="cluster-worker-id">{{ worker.worker_id }}</strong>
                <el-tag size="small" :type="worker.online ? 'success' : 'info'">
                  {{ worker.online ? $t('cluster.workerOnline') : $t('cluster.workerOffline') }}
                </el-tag>
              </div>
              <p>
                {{ $t('cluster.workerTasks', { tasks: worker.task_count, running: worker.running, leased: worker.leased }) }}
              </p>
              <p class="cluster-worker-time">
                {{ $t('cluster.lastHeartbeat', { time: formatTs(worker.last_seen_at) }) }}
              </p>
            </div>
            <el-empty
              v-if="workerRows.length === 0"
              data-testid="workers-empty"
              :description="$t('cluster.noWorkers')"
              :image-size="56"
            />
          </div>
          <div v-else class="overview-note">
            {{ $t('cluster.overviewNote') }}
          </div>
        </el-card>

        <el-card
          v-if="activeView === 'overview' || activeView === 'sources'"
          shadow="never"
          class="panel-card"
        >
          <template #header>
            <div class="panel-title">
              <span><i class="fa-solid fa-network-wired" /> {{ $t('panel.sourceCoverage') }}</span>
              <span class="panel-hint">{{ $t('panel.hosts', { count: dashboard.sources.length }) }}</span>
            </div>
          </template>

          <div class="source-board" v-if="dashboard.sources.length > 0">
            <div
              v-for="item in dashboard.sources"
              :key="`${item.host}:${item.port}`"
              class="source-cell"
            >
              <p class="source-name">{{ item.host }}:{{ item.port }}</p>
              <p class="source-stats source-stats--summary">
                {{ $t('source.stats', { tasks: item.task_count, running: item.running, normal: item.normal, delayed: item.delayed, abnormal: item.abnormal }) }}
              </p>
            </div>
          </div>
          <el-empty
            v-else
            data-testid="sources-empty"
            :description="$t('empty.noSources')"
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
                {{ activeView === "alerts" ? $t('nav.alerts') : $t('nav.tasks') }}
              </span>
              <div class="panel-title-actions">
                <span class="panel-hint">
                  {{
                    activeView === "alerts"
                      ? $t('filter.alertFilteredCount', { filtered: filteredTasks.length, total: dashboard.tasks.length })
                      : $t('filter.filteredCount', { filtered: filteredTasks.length, total: dashboard.tasks.length })
                  }}
                </span>
                <el-button size="small" @click="openBatchCreate">
                  <i class="fa-solid fa-layer-group" /> {{ $t('btn.batchCreate') }}
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
            :row-attr="() => ({ tabindex: '0' })"
            @row-click="onRowClick"
            @keydown.enter="onTableKeyEnter"
          >
            <el-table-column :label="$t('table.id')" width="70">
              <template #default="{ row }"><span class="task-id-cell" :data-testid="`task-row-${row.task.id}`">{{ row.task.id }}</span></template>
            </el-table-column>
            <el-table-column :label="$t('table.name')" min-width="180">
              <template #default="{ row }"><span class="task-name-cell">{{ row.task.name }}</span></template>
            </el-table-column>
            <el-table-column :label="$t('table.taskState')" width="140">
              <template #default="{ row }">
                <el-tag size="small" :type="stateTagType(row.task.state)">{{ stateLabel(row.task.state) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column :label="$t('table.ownerWorker')" min-width="130">
              <template #default="{ row }">{{ ownerWorkerLabel(row.task) }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.leaseRisk')" width="100">
              <template #default="{ row }">
                <el-tag size="small" :type="leaseRiskTagType(row.task)">{{ leaseRiskLabel(row.task) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column :label="$t('table.replicationStatus')" width="170">
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
            <el-table-column :label="$t('table.delay')" width="100">
              <template #default="{ row }"><span class="delay-cell">{{ formatDelay(row.replication.delay_seconds, row.replication.has_progress) }}</span></template>
            </el-table-column>
            <el-table-column :label="$t('table.source')" min-width="170">
              <template #default="{ row }">{{ sourceLabel(row.task) }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.lastEventTime')" min-width="170">
              <template #default="{ row }">{{ formatTs(row.replication.last_event_at) }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.semiSync')" width="90">
              <template #default="{ row }">{{ row.task.source?.semi_sync ? $t('detail.on') : $t('detail.off') }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.actions')" width="110" fixed="right" class-name="action-col">
              <template #default="{ row }">
                <div class="action-row">
                  <el-button class="action-btn" size="small" :data-testid="`task-detail-trigger-${row.task.id}`" @click.stop="showDetail(row.task)">{{ $t('btn.detail') }}</el-button>
                </div>
              </template>
            </el-table-column>
          </el-table>
          <el-empty
            v-if="pagedTasks.length === 0"
            data-testid="task-table-empty"
            :description="$t('empty.noTasks')"
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

    <TaskCreateDialog
      v-model:visible="formVisible"
      :is-mobile="isMobile"
      :mode="formMode"
      :form="form"
      @submit="submitForm"
    />

    <BatchCreateDialog
      v-model:visible="batchVisible"
      :is-mobile="isMobile"
      :batch-form="batchForm"
      :batch-preview="batchPreview"
      @preview="previewBatchCreate"
      @submit="submitBatchCreate"
    />

    <TaskDetailDrawer
      v-model:visible="detailVisible"
      :task="detailTask"
      :replication="detailReplication"
      :lease="detailLease"
      :checkpoint="checkpoint"
      :files="files"
      :runs-limited="detailRunsLimited"
      :events="events"
      :run-history-limit="runHistoryLimit"
      :state-tag-type="stateTagType"
      :state-label="stateLabel"
      :replication-tag-type="replicationTagType"
      :replication-status-label="replicationStatusLabel"
      :source-label="sourceLabel"
      :lease-risk-label="leaseRiskLabel"
      :format-delay="formatDelay"
      :format-checkpoint="formatCheckpoint"
      :format-replication-reason="formatReplicationReason"
      :format-ts="formatTs"
      @edit="openEdit"
      @start="onStart"
      @stop="onStop"
      @delete="onDelete"
      @retry-upload="retryFailedUploads"
      :is-mobile="isMobile"
    />

    <!-- Settings Dialog -->
    <SettingsDialog
      v-model:visible="settingsVisible"
      v-model:token="settingsToken"
      :is-mobile="isMobile"
      :current-locale="currentLocale"
      @locale-change="onLocaleChange"
      @save="saveSettings"
    />
  </div>
  </el-config-provider>
</template>

<script setup>
import { computed, reactive, ref, watch, onMounted, onBeforeUnmount } from "vue";
import { useI18n } from "vue-i18n";
import zhCnLocale from "element-plus/dist/locale/zh-cn.mjs";
import enLocale from "element-plus/dist/locale/en.mjs";
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
import { setLocale, getLocale } from "./locales";
import { useWindowState } from "./composables/useWindowState.js";
import { useAuth } from "./composables/useAuth.js";
import { useDashboard } from "./composables/useDashboard.js";
import { useSourceLookup } from "./composables/useSourceLookup.js";
import { useTaskFilter } from "./composables/useTaskFilter.js";
import { useTaskDetail } from "./composables/useTaskDetail.js";
import { useTaskForm } from "./composables/useTaskForm.js";
import { useBatchCreate } from "./composables/useBatchCreate.js";
import { useFormatters } from "./composables/useFormatters.js";
import AlertBanner from "./components/AlertBanner.vue";
import MetricGrid from "./components/MetricGrid.vue";
import NavPane from "./components/NavPane.vue";
import AppHeader from "./components/AppHeader.vue";
import SettingsDialog from "./components/SettingsDialog.vue";
import TaskCreateDialog from "./components/TaskCreateDialog.vue";
import BatchCreateDialog from "./components/BatchCreateDialog.vue";
import TaskDetailDrawer from "./components/TaskDetailDrawer.vue";

const { t } = useI18n();

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
const currentLocale = ref(getLocale());
const elLocale = computed(() => currentLocale.value === "zh-CN" ? zhCnLocale : enLocale);
const { settingsVisible, settingsToken, authRequiredMessage, authRequiredTitle, openSettings, saveSettings } = useAuth(() => refreshAll());
const activeView = ref(resolveViewFromHash());

// Locale change handler
function onLocaleChange(locale) {
  setLocale(locale);
  window.location.reload();
}
const { menuCollapsed, windowWidth, isMobile } = useWindowState();

// URL route listener
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
  window.addEventListener("hashchange", handleHashChange);
  handleHashChange();
  syncHash(activeView.value, true);
});

onBeforeUnmount(() => {
  window.removeEventListener("hashchange", handleHashChange);
});

const { loading, dashboard, cluster, toTimeMs, nowRefMs, applyDashboardData, applyClusterData, buildSourceFilter } = useDashboard();

const { sourceQuery, lookup, clearLookupState } = useSourceLookup();

const {
  uiFilter, pager, activeQuickFilter,
  filteredTasks, pagedTasks,
  taskStates, replicationStatuses,
  stateLabel, replicationStatusLabel, sourceLabel,
  debouncedKeyword, debouncedSourceKeyword,
  resetUiFilter,
} = useTaskFilter(dashboard);

const {
  ownerWorkerLabel, leaseRiskTagType, leaseRiskLabel,
  stateTagType, replicationTagType,
  formatDelay, formatTs, formatCheckpoint,
  formatReplicationReason, hasReplicationReason, parseErr,
} = useFormatters({ cluster, toTimeMs, nowRefMs, currentLocale });

const {
  formVisible, formMode, form,
  openCreate, openEdit, buildPayload, validateTaskPayload, resetForm,
} = useTaskForm({ refreshAll, parseErr });

const {
  batchVisible, batchForm, batchPreview,
  openBatchCreate, previewBatchCreate, submitBatchCreate, clearBatchPreview,
} = useBatchCreate({ refreshAll, validateTaskPayload, parseErr });

const {
  detailVisible, detailTask, detailReplication, detailLease,
  detailRuns, runHistoryLimit, checkpoint, events, files,
  showDetail,
} = useTaskDetail(cluster);

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
    debouncedKeyword.value,
    debouncedSourceKeyword.value,
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
      getDashboard(buildSourceFilter(sourceQuery)),
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
    ElMessage.error(t("msg.hostPortRequired"));
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
  clearLookupState();
  await refreshAll();
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
      ElMessage.success(t("msg.taskCreated"));
    } else {
      await updateTask(form.id, payload);
      ElMessage.success(t("msg.taskUpdated"));
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
    ElMessage.success(t("msg.taskStarted", { id: task.id }));
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onStop(task) {
  try {
    await stopTask(task.id);
    ElMessage.success(t("msg.taskStopped", { id: task.id }));
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onDelete(task) {
  try {
    await ElMessageBox.confirm(t("msg.confirmDelete", { id: task.id }), t("msg.deleteConfirmTitle"), { type: "warning" });
    await deleteTask(task.id);
    ElMessage.success(t("msg.taskDeleted", { id: task.id }));
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

function onTableKeyEnter(e) {
  const tr = e.target.closest("tr");
  if (!tr) return;
  const trs = Array.from(tr.closest("tbody")?.querySelectorAll("tr") || []);
  const idx = trs.indexOf(tr);
  if (idx >= 0 && idx < pagedTasks.value.length) onRowClick(pagedTasks.value[idx]);
}

async function retryFailedUploads(task) {
  try {
    await retryUpload(task.id, 100);
    files.value = await listFiles(task.id, 80);
    ElMessage.success(t("msg.retryUploadTriggered"));
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}


</script>

<style>
.page-shell {
  --bg: #f5f5f4;
  --surface: #ffffff;
  --surface-soft: #f8f8f7;
  --surface-raised: #fcfcfb;
  --line: #e7e5e4;
  --line-strong: #d6d3d1;
  --text: #111827;
  --text-secondary: #374151;
  --text-tertiary: #4b5563;
  --text-muted: #9ca3af;
  --text-label: #52525b;
  --text-dim: #71717a;
  --text-strong: #18181b;
  --sub: #6b7280;
  --accent: #111827;
  --accent-hover: #1f2937;
  --surface-page: #fafaf9;
  --surface-card: #fdfdfc;
  --text-heading: #3f3f46;
  --line-connector: #d4d4d8;

  max-width: 1720px;
  margin: 0 auto;
  min-height: 100vh;
  padding: 24px 24px 24px 252px;
  font-family: "Geist", "SF Pro Display", "PingFang SC", sans-serif;
  color: var(--text);
  background: linear-gradient(180deg, var(--surface-page) 0%, var(--bg) 100%);
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
  align-items: flex-start;
  gap: 20px;
  margin-bottom: 14px;
  padding-bottom: 6px;
  animation: rise-fade 0.36s ease both;
}

.hero-copy {
  max-width: 760px;
}

.kicker {
  margin: 0 0 2px;
  color: var(--sub);
  letter-spacing: 0.14em;
  font-size: 10px;
  font-weight: 700;
}

.kicker i {
  margin-right: 6px;
}

h1 {
  margin: 0 0 6px;
  font-size: 30px;
  line-height: 1.08;
  letter-spacing: -0.03em;
  font-weight: 640;
}

.hero-desc {
  margin: 0;
  color: var(--sub);
  max-width: 520px;
  font-size: 14px;
  line-height: 1.55;
}

.hero-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 8px;
  padding-top: 2px;
}

.hero-actions i {
  margin-right: 6px;
}

.hero-actions .el-button {
  height: 34px;
  padding-inline: 13px;
  font-weight: 600;
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(6, minmax(130px, 1fr));
  gap: 10px;
  margin-bottom: 16px;
}

.metric-card {
  border: 1px solid var(--line);
  background: var(--surface-raised);
  border-radius: 12px;
  padding: 13px 14px;
  box-shadow: 0 1px 2px rgba(17, 24, 39, 0.025);
  animation: rise-fade 0.42s ease both;
  cursor: pointer;
  transition:
    transform 0.18s ease,
    border-color 0.18s ease,
    box-shadow 0.18s ease,
    background-color 0.18s ease;
}

.metric-card p {
  margin: 0;
  color: var(--sub);
  font-size: 12px;
  font-weight: 600;
  letter-spacing: 0.01em;
  display: flex;
  align-items: center;
  gap: 6px;
}

.metric-card i {
  color: var(--text-muted);
}

.metric-card strong {
  display: block;
  margin-top: 12px;
  font-size: 28px;
  line-height: 1;
  letter-spacing: -0.03em;
  color: var(--text);
  font-variant-numeric: tabular-nums;
}

.metric-card:hover {
  transform: translateY(-1px);
  border-color: var(--line-strong);
  box-shadow: 0 6px 14px rgba(17, 24, 39, 0.04);
}

.metric-card[data-active='true'] {
  border-color: #cbd5e1;
  background: var(--surface);
  box-shadow: 0 0 0 1px rgba(148, 163, 184, 0.14);
}

.metric-card--danger {
  border-color: #efcaca;
  background: #fff8f8;
}

.metric-card--danger p,
.metric-card--danger i {
  color: #b42318;
}

.metric-card--danger strong {
  color: #991b1b;
}

.metric-card--warning {
  border-color: #efdfa8;
  background: #fffdf7;
}

.metric-card--warning p,
.metric-card--warning i {
  color: #9a6700;
}

.metric-card--warning strong {
  color: #92400e;
}

.metric-card--success {
  border-color: #e3e8e3;
  background: #fbfcfb;
}

.metric-card--success p,
.metric-card--success i {
  color: var(--sub);
}

.metric-card--success strong {
  color: #166534;
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
  border-right: 1px solid #e5e7eb;
  border-radius: 0;
  background: linear-gradient(180deg, #f7f7f6 0%, #f3f4f6 100%);
  padding: 14px 10px 10px;
  display: flex;
  flex-direction: column;
  gap: 5px;
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
  margin-bottom: 6px;
  padding: 0 2px;
}

.nav-foot {
  margin-top: auto;
  padding-top: 10px;
}

.nav-pane--collapsed .nav-head {
  justify-content: center;
}

.nav-brand {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: var(--text);
  min-height: 30px;
}

.nav-brand-icon {
  width: 24px;
  height: 24px;
  border-radius: 7px;
  background: var(--line);
  display: inline-flex;
  align-items: center;
  justify-content: center;
  color: var(--text-label);
  font-size: 12px;
}

.nav-brand-text {
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.04em;
  color: var(--text-secondary);
}

.nav-pane--collapsed .nav-brand-text {
  display: none;
}

.nav-collapse-btn {
  border: 0;
  background: transparent;
  color: var(--sub);
  border-radius: 8px;
  width: 30px;
  height: 30px;
  cursor: pointer;
  transition: background-color 0.15s ease, color 0.15s ease;
}

.nav-collapse-btn:hover {
  background: rgba(255, 255, 255, 0.6);
  color: var(--text-secondary);
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
  border: 1px solid transparent;
  border-radius: 10px;
  background: transparent;
  color: var(--text-secondary);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 6px;
  padding: 8px 10px;
  font-size: 13px;
  font-weight: 550;
  cursor: pointer;
  transition:
    background 0.15s ease,
    color 0.15s ease,
    box-shadow 0.15s ease,
    border-color 0.15s ease;
}

.nav-item span {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.nav-item i {
  color: var(--text-muted);
}

.nav-item--active {
  background: rgba(255, 255, 255, 0.88);
  border-color: rgba(148, 163, 184, 0.2);
  color: var(--text);
  font-weight: 600;
  box-shadow: 0 1px 2px rgba(17, 24, 39, 0.03);
}

.nav-item--active i {
  color: var(--text-tertiary);
}

.nav-item:hover {
  background: rgba(255, 255, 255, 0.62);
  border-color: rgba(229, 231, 235, 0.9);
}

.nav-pane--collapsed .nav-item {
  width: 40px;
  height: 40px;
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
  color: var(--text-muted);
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
  box-shadow: 0 1px 2px rgba(17, 24, 39, 0.03);
  transition:
    transform 0.18s ease,
    box-shadow 0.18s ease,
    border-color 0.18s ease,
    background-color 0.18s ease;
  animation: rise-fade 0.5s ease both;
}

.panel-card:hover {
  transform: translateY(-1px);
  border-color: var(--line-strong);
  box-shadow: 0 6px 16px rgba(17, 24, 39, 0.04);
}

.panel-card--secondary {
  background: var(--surface-raised);
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
  color: var(--text-label);
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
  font-family: "IBM Plex Mono", monospace;
  letter-spacing: 0.01em;
}

.switch-row {
  border: 1px dashed var(--line-strong);
  border-radius: 12px;
  padding: 12px;
  display: flex;
  justify-content: space-between;
  align-items: center;
  color: var(--sub);
  background: var(--surface-raised);
}

.filter-chip-row .el-button {
  min-height: 32px;
  padding-inline: 12px;
  border-radius: 999px;
  font-weight: 600;
}

.filter-stack .el-input__wrapper,
.filter-stack .el-select__wrapper {
  min-height: 38px;
  background: var(--surface-raised);
}

.filter-stack .el-switch__core {
  border-color: var(--line-strong);
  background: var(--line);
}

.filter-stack .el-switch.is-checked .el-switch__core {
  border-color: var(--text);
  background: var(--accent);
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
  border-radius: 12px;
  padding: 12px 12px 14px;
  background: var(--surface-raised);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.78);
}

.cluster-stat-cell p {
  margin: 0;
  color: var(--sub);
  font-size: 12px;
  letter-spacing: 0.01em;
}

.cluster-stat-value {
  margin-top: 10px;
  display: block;
  font-size: 28px;
  line-height: 1;
  letter-spacing: -0.03em;
  color: var(--text);
  font-family: "IBM Plex Mono", monospace;
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
  padding: 12px;
  background: var(--surface-raised);
  box-shadow: 0 1px 2px rgba(17, 24, 39, 0.02);
}

.cluster-worker-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}

.cluster-worker-id {
  color: var(--text);
  font-size: 13px;
  letter-spacing: -0.01em;
}

.cluster-worker-item p {
  margin: 8px 0 0;
  color: var(--sub);
  font-size: 12px;
}

.cluster-worker-time {
  font-family: "IBM Plex Mono", monospace;
  color: var(--sub);
}

.overview-note {
  margin-top: 12px;
  border: 1px dashed var(--line-strong);
  border-radius: 12px;
  padding: 12px;
  color: var(--sub);
  font-size: 12px;
  background: var(--surface-raised);
}

.source-cell {
  border: 1px solid var(--line);
  border-radius: 12px;
  padding: 12px;
  background: var(--surface-raised);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.78);
}

.source-name {
  margin: 0;
  font-weight: 600;
  color: var(--text);
  line-height: 1.35;
}

.source-stats {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--sub);
}

.source-stats--summary {
  line-height: 1.6;
}

.table-card .el-table {
  border-radius: 12px;
  overflow: hidden;
  --el-table-border-color: var(--line);
  --el-table-row-hover-bg-color: var(--bg);
  --el-table-header-bg-color: var(--surface-soft);
  --el-table-tr-bg-color: var(--surface);
}

.table-card .el-table th.el-table__cell {
  background: var(--surface-soft);
  color: var(--text-tertiary);
  font-weight: 600;
  font-size: 12px;
  letter-spacing: 0.02em;
}

.table-card .el-table td.el-table__cell,
.table-card .el-table th.el-table__cell {
  border-bottom-color: var(--line);
  padding-top: 11px;
  padding-bottom: 11px;
}

.table-card .el-table .cell {
  line-height: 1.45;
}

.table-card .el-table td.el-table__cell {
  color: var(--text-secondary);
}

.task-id-cell {
  color: var(--sub);
  font-family: "IBM Plex Mono", monospace;
  font-size: 12px;
}

.table-card .el-table__inner-wrapper::before {
  background-color: var(--line);
}

.table-card .el-table__body tr:hover > td.el-table__cell {
  background: var(--surface-page);
}

.table-card .el-table__body tr {
  cursor: pointer;
}

.task-name-cell {
  font-weight: 600;
  color: var(--text);
}

.delay-cell {
  font-family: "IBM Plex Mono", monospace;
  color: var(--text-secondary);
}

.replication-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}

.reason-tip-icon {
  color: var(--text-muted);
  font-size: 12px;
  cursor: help;
  transition: color 0.15s ease;
}

.reason-tip-icon:hover {
  color: var(--text-tertiary);
}

.action-row {
  display: inline-flex;
  gap: 4px;
  flex-wrap: nowrap;
  white-space: nowrap;
}

.table-card .action-col .cell {
  white-space: nowrap;
  overflow: visible;
}

.table-card .action-row .el-button + .el-button {
  margin-left: 0;
}

.table-card .action-btn {
  border-color: transparent;
  background: transparent;
  color: var(--sub);
  font-weight: 500;
}

.table-card .action-btn:hover {
  border-color: var(--line);
  background: var(--surface);
  color: var(--text);
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
  background: linear-gradient(180deg, var(--surface-card) 0%, var(--surface-page) 100%);
  padding: 14px;
}

.detail-hero {
  display: flex;
  justify-content: space-between;
  gap: 18px;
  flex-wrap: wrap;
}

.detail-hero h3 {
  margin-bottom: 0;
  font-size: 20px;
  color: var(--text);
}

.detail-hero-kicker {
  color: var(--sub);
  font-size: 11px;
  letter-spacing: 0.12em;
  font-weight: 700;
  margin-bottom: 8px;
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

.detail-hero-meta span {
  display: inline-flex;
  align-items: center;
  min-height: 26px;
  padding: 0 10px;
  border-radius: 999px;
  border: 1px solid var(--line);
  background: rgba(255, 255, 255, 0.72);
}

.detail-action-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: flex-start;
  padding-top: 2px;
}

.detail-action-row .el-button {
  min-width: 88px;
  min-height: 34px;
}

.detail-action-row .el-button--danger {
  margin-left: 8px;
}

.detail-grid--summary {
  margin-top: 14px;
}

.detail-panel {
  border: 1px solid var(--line);
  border-radius: 12px;
  background: var(--surface);
  padding: 12px;
  box-shadow: 0 1px 2px rgba(17, 24, 39, 0.02);
}

.detail-panel h3 {
  margin: 0 0 12px;
  font-size: 14px;
  color: var(--text-heading);
  display: flex;
  align-items: center;
  gap: 8px;
}

.detail-panel-toolbar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: 10px;
}

.detail-panel .el-table {
  border-radius: 12px;
  overflow: hidden;
  --el-table-border-color: var(--line);
  --el-table-header-bg-color: var(--surface-soft);
  --el-table-row-hover-bg-color: var(--bg);
}

.detail-panel .el-table th.el-table__cell {
  background: var(--surface-soft);
  color: var(--text-label);
  font-size: 12px;
  font-weight: 600;
}

.detail-panel .el-table td.el-table__cell,
.detail-panel .el-table th.el-table__cell {
  border-bottom-color: var(--line);
  padding-top: 9px;
  padding-bottom: 9px;
}

.detail-panel .el-timeline-item__node {
  background-color: var(--line-connector);
}

.detail-panel .el-timeline-item__tail {
  border-left-color: #e7e5e4;
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
  background: var(--surface-card);
}

.detail-item span {
  display: block;
  color: var(--text-dim);
  font-size: 12px;
}

.detail-item strong {
  margin-top: 6px;
  display: block;
  color: var(--text-strong);
  word-break: break-word;
}

.form-hint {
  margin-top: 6px;
  color: var(--text-dim);
  font-size: 12px;
  line-height: 1.5;
}

.el-card__header {
  border-bottom-color: var(--line);
}

.el-card__body {
  padding: 14px;
}

.task-detail-drawer {
  background: var(--surface-raised);
}

.task-detail-drawer .el-drawer__header {
  margin-bottom: 0;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--line);
}

.task-detail-drawer .el-drawer__title {
  color: var(--text);
  font-size: 15px;
  font-weight: 700;
}

.task-detail-drawer .el-drawer__body {
  padding: 16px 20px 20px;
}

.settings-dialog {
  border-radius: 16px;
  overflow: hidden;
}

.settings-dialog .el-dialog__header {
  margin-right: 0;
  padding: 18px 20px 14px;
  border-bottom: 1px solid var(--line);
  background: var(--surface-raised);
}

.settings-dialog .el-dialog__title {
  color: var(--text);
  font-size: 15px;
  font-weight: 700;
}

.settings-dialog .el-dialog__body {
  padding: 18px 20px 14px;
}

.settings-form {
  display: grid;
  gap: 4px;
}

.settings-dialog .el-form-item {
  margin-bottom: 16px;
}

.settings-dialog .el-form-item:last-child {
  margin-bottom: 0;
}

.settings-dialog .el-form-item__label {
  color: var(--text-label);
  font-weight: 600;
}

.settings-dialog .el-input__wrapper,
.settings-dialog .el-select__wrapper {
  min-height: 40px;
  background: var(--surface-raised);
}

.settings-dialog .el-dialog__footer {
  padding: 12px 20px 18px;
  border-top: 1px solid var(--line);
  background: var(--surface-raised);
}

.el-input__wrapper,
.el-select__wrapper,
.el-input-number .el-input__wrapper {
  border-radius: 10px;
  background: #fff;
  border: 1px solid var(--line);
  box-shadow: none;
}

.el-input__wrapper:hover,
.el-select__wrapper:hover,
.el-input-number .el-input__wrapper:hover {
  border-color: var(--line-strong);
}

.el-input__wrapper.is-focus,
.el-select__wrapper.is-focused,
.el-input-number .el-input__wrapper.is-focus {
  box-shadow: 0 0 0 1px #111827 inset;
}

.el-button {
  border-radius: 10px;
  font-weight: 500;
}
.el-button--primary {
  background: var(--accent);
  border-color: var(--text);
  color: #fff;
}

.el-button--primary:hover {
  background: var(--accent-hover);
  border-color: var(--accent-hover);
}

.el-button:not(.el-button--primary):not(.el-button--success):not(.el-button--warning):not(.el-button--danger) {
  border-color: var(--line);
  background: #fff;
  color: var(--text-secondary);
}

.el-button:not(.el-button--primary):not(.el-button--success):not(.el-button--warning):not(.el-button--danger):hover {
  border-color: var(--line-strong);
  background: var(--surface-page);
  color: var(--text);
}

.table-card .action-btn {
  min-width: 56px;
  height: 28px;
  padding-left: 10px;
  padding-right: 10px;
  border-radius: 999px;
  font-weight: 600;
  font-size: 11px;
  letter-spacing: 0.01em;
}

.table-card .action-btn.el-button--success {
  background: #f0fdf4;
  border-color: #bbf7d0;
  color: #166534;
}

.table-card .action-btn.el-button--warning {
  background: #fffbeb;
  border-color: #fde68a;
  color: #92400e;
}

.table-card .action-btn.el-button--danger {
  background: #fef2f2;
  border-color: #fecaca;
  color: #991b1b;
}

.metric-card:focus-visible {
  outline: 2px solid #111827;
  outline-offset: 2px;
}

.el-tag {
  border-radius: 999px;
  font-weight: 600;
  letter-spacing: 0.01em;
}

.el-tag--success {
  background: #f4fbf6;
  border-color: #cfe8d6;
  color: #166534;
}

.el-tag--warning {
  background: #fffaf0;
  border-color: #efd99a;
  color: #92400e;
}

.el-tag--danger {
  background: #fff4f4;
  border-color: #f1c7c7;
  color: #991b1b;
}

.el-tag--info {
  background: #f5f5f4;
  border-color: var(--line);
  color: var(--text-label);
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
    font-size: 28px;
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
    background: linear-gradient(180deg, rgba(250, 250, 249, 0.96) 0%, rgba(244, 244, 243, 0.98) 100%);
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

@media (max-width: 767px) {
  .page-shell {
    padding: 10px;
  }

  .hero {
    padding: 14px 0 10px;
  }

  h1 {
    font-size: 22px;
  }

  .metric-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px;
  }

  .nav-pane {
    flex-direction: column;
    gap: 4px;
  }

  .nav-collapse-btn--dock {
    display: none;
  }

  .action-row {
    flex-wrap: wrap;
    gap: 4px;
  }

  .action-row .el-button {
    flex: 1 1 auto;
    min-width: 80px;
  }

  .hero-actions {
    flex-wrap: wrap;
    gap: 6px;
  }

  .detail-grid {
    grid-template-columns: 1fr;
  }
}
</style>
