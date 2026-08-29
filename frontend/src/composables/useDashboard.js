// input: API layer (getDashboard, getClusterOverview, listWorkers, getTaskLease)
// output: dashboard + cluster reactive state, server pagination metadata, legacy page fallback, status counters including STARTING, loading flag, refresh helpers, nowRefMs
// pos: central data layer composable; sourceQuery/lookup live in useSourceLookup
// note: if this file changes, update this header and frontend/src/README.md
import { reactive, ref } from "vue";
import { ElMessage } from "element-plus";
import {
  getDashboard,
  getClusterOverview,
  listWorkers,
  getTaskLease,
} from "../api";

export function useDashboard() {
  const loading = ref(false);

  const dashboard = reactive({
    generated_at: "",
    threshold_seconds: 30,
    total: 0,
    limit: 100,
    offset: 0,
    has_pagination: false,
    summary: {
      total: 0,
      starting: 0,
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

  function applyDashboardData(data) {
    if (!data) return;
    const tasks = Array.isArray(data.tasks) ? data.tasks : [];
    const summaryTotal = data.summary?.total;
    const total = data.total === undefined ? (summaryTotal ?? tasks.length) : data.total;
    if (!Number.isInteger(total) || total < 0) {
      throw new Error("invalid dashboard total");
    }
    const limit = data.limit === undefined ? 100 : data.limit;
    if (!Number.isInteger(limit) || limit <= 0) {
      throw new Error("invalid dashboard limit");
    }
    const offset = data.offset === undefined ? 0 : data.offset;
    if (!Number.isInteger(offset) || offset < 0) {
      throw new Error("invalid dashboard offset");
    }
    dashboard.generated_at = data.generated_at || "";
    dashboard.threshold_seconds = data.threshold_seconds || 30;
    dashboard.total = total;
    dashboard.limit = limit;
    dashboard.offset = offset;
    dashboard.has_pagination =
      data.total !== undefined && data.limit !== undefined && data.offset !== undefined;
    Object.assign(dashboard.summary, data.summary || {}, { starting: data.summary?.starting ?? 0 });
    dashboard.tasks = tasks;
    dashboard.sources = Array.isArray(data.sources) ? data.sources : [];
  }

  function applyClusterData(overview, workers) {
    if (overview) Object.assign(cluster.overview, overview);
    cluster.workers = Array.isArray(workers) ? workers : [];
  }

  function buildSourceFilter(sourceQuery) {
    const params = {};
    if (sourceQuery?.host?.trim()) params.host = sourceQuery.host.trim();
    if (sourceQuery?.port) params.port = Number(sourceQuery.port);
    return params;
  }

  async function prefetchLeasesForIds(ids) {
    if (!ids.length) return;
    const results = await Promise.allSettled(ids.map((id) => getTaskLease(id)));
    results.forEach((result, idx) => {
      const id = ids[idx];
      if (result.status === "fulfilled") {
        cluster.leaseByTask[id] = result.value;
      }
    });
  }

  async function refreshAll(sourceQuery, onAfterRefresh) {
    try {
      loading.value = true;
      const params = buildSourceFilter(sourceQuery);
      const [dashboardData, overviewData, workersData] = await Promise.all([
        getDashboard(params),
        getClusterOverview(),
        listWorkers(),
      ]);
      applyDashboardData(dashboardData);
      applyClusterData(overviewData, workersData);
      if (onAfterRefresh) await onAfterRefresh();
    } catch (err) {
      ElMessage.error(err?.message || String(err));
    } finally {
      loading.value = false;
    }
  }

  return {
    loading,
    dashboard,
    cluster,
    toTimeMs,
    nowRefMs,
    applyDashboardData,
    applyClusterData,
    buildSourceFilter,
    prefetchLeasesForIds,
    refreshAll,
  };
}
