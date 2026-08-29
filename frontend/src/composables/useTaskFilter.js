// input: dashboard task page plus total/compatibility metadata, i18n t(), sourceLabel helper
// output: uiFilter state, server pager/query builder, current-page filteredTasks, pagedTasks, quick filter actions
// pos: task list filtering and server pagination coordination
// note: if this file changes, update this header and frontend/src/composables/README.md.
import { computed, reactive, ref, watch } from "vue";
import { useI18n } from "vue-i18n";

export function useTaskFilter(dashboard) {
  const { t } = useI18n();

  const taskStates = [
    "CREATED",
    "STARTING",
    "RUNNING",
    "LEASE_DEGRADED",
    "REBUILDING_FILE",
    "RETRY_BACKOFF",
    "STOPPING",
    "STOPPED",
    "FAILED",
  ];
  const replicationStatuses = ["NORMAL", "DELAYED", "ABNORMAL", "IDLE"];

  function stateLabel(state) {
    return t(`state.${state}`) || state || "--";
  }

  function replicationStatusLabel(status) {
    return t(`replication.${status}`) || status || "--";
  }

  function sourceLabel(task) {
    return `${task?.source?.host || "-"}:${task?.source?.port || "-"}`;
  }

  const uiFilter = reactive({
    keyword: "",
    sourceKeyword: "",
    taskState: "ALL",
    replicationStatus: "ALL",
    sortBy: "delay_desc",
    onlyAlert: false,
  });

  const pager = reactive({ page: 1, pageSize: 20 });

  const debouncedKeyword = ref("");
  const debouncedSourceKeyword = ref("");
  let _kwTimer = null;
  let _srcTimer = null;
  watch(
    () => uiFilter.keyword,
    (v) => {
      clearTimeout(_kwTimer);
      _kwTimer = setTimeout(() => {
        debouncedKeyword.value = v;
      }, 300);
    },
    { immediate: true },
  );
  watch(
    () => uiFilter.sourceKeyword,
    (v) => {
      clearTimeout(_srcTimer);
      _srcTimer = setTimeout(() => {
        debouncedSourceKeyword.value = v;
      }, 300);
    },
    { immediate: true },
  );

  const filteredTasks = computed(() => {
    let rows = [...(dashboard.tasks || [])];

    if (debouncedKeyword.value.trim()) {
      const kw = debouncedKeyword.value.trim().toLowerCase();
      rows = rows.filter((row) => {
        const id = String(row.task?.id || "").toLowerCase();
        const name = String(row.task?.name || "").toLowerCase();
        return id.includes(kw) || name.includes(kw);
      });
    }

    if (debouncedSourceKeyword.value.trim()) {
      const sourceKw = debouncedSourceKeyword.value.trim().toLowerCase();
      rows = rows.filter((row) =>
        sourceLabel(row.task).toLowerCase().includes(sourceKw),
      );
    }

    if (uiFilter.taskState !== "ALL") {
      rows = rows.filter((row) => row.task?.state === uiFilter.taskState);
    }

    if (uiFilter.replicationStatus !== "ALL") {
      rows = rows.filter(
        (row) => row.replication?.status === uiFilter.replicationStatus,
      );
    }

    if (uiFilter.onlyAlert) {
      rows = rows.filter((row) => {
        const rep = row.replication?.status;
        const taskState = row.task?.state;
        return (
          rep === "ABNORMAL" ||
          rep === "DELAYED" ||
          taskState === "FAILED" ||
          taskState === "RETRY_BACKOFF"
        );
      });
    }

    rows.sort((a, b) => {
      if (uiFilter.sortBy === "name_asc") {
        return String(a.task?.name || "").localeCompare(
          String(b.task?.name || ""),
        );
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
    if (dashboard.has_pagination) return filteredTasks.value;
    const start = (pager.page - 1) * pager.pageSize;
    return filteredTasks.value.slice(start, start + pager.pageSize);
  });

  const serverTotal = computed(() => dashboard.total ?? dashboard.summary?.total ?? 0);

  function buildPaginationParams() {
    const params = {
      limit: pager.pageSize,
      offset: (pager.page - 1) * pager.pageSize,
    };
    if (uiFilter.taskState !== "ALL") params.state = uiFilter.taskState;
    return params;
  }

  watch(
    () => [
      debouncedKeyword.value,
      debouncedSourceKeyword.value,
      uiFilter.taskState,
      uiFilter.replicationStatus,
      uiFilter.sortBy,
      uiFilter.onlyAlert,
    ],
    () => {
      pager.page = 1;
    },
  );

  watch(
    () => [serverTotal.value, pager.pageSize],
    () => {
      const maxPage = Math.max(
        1,
        Math.ceil(serverTotal.value / pager.pageSize),
      );
      if (pager.page > maxPage) pager.page = maxPage;
    },
  );

  const activeQuickFilter = ref("all");

  function resetUiFilter() {
    uiFilter.keyword = "";
    uiFilter.sourceKeyword = "";
    uiFilter.taskState = "ALL";
    uiFilter.replicationStatus = "ALL";
    uiFilter.sortBy = "delay_desc";
    uiFilter.onlyAlert = false;
    activeQuickFilter.value = "all";
  }

  return {
    taskStates,
    replicationStatuses,
    stateLabel,
    replicationStatusLabel,
    sourceLabel,
    uiFilter,
    pager,
    serverTotal,
    buildPaginationParams,
    activeQuickFilter,
    filteredTasks,
    pagedTasks,
    debouncedKeyword,
    debouncedSourceKeyword,
    resetUiFilter,
  };
}
