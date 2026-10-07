// input: API calls for task data including single-task GET /lease, GET /replay, and GET /window
// output: detail drawer state, including the recoverable window, the replay set, the inventory limit shared with replay archive download, and showDetail action
// pos: task detail drawer data management; /lease stays on this single-task path only
// note: if this file changes, update this header and frontend/src/composables/README.md
import { ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage } from "element-plus";
import {
  getTask,
  getCheckpoint,
  listEvents,
  listFiles,
  listReplay,
  getRecoveryWindow,
  getReplication,
  getTaskLease,
  listTaskRuns,
} from "../api.js";

const RUN_HISTORY_LIMIT = 10;

// taskDetailInventoryLimit is the files/replay window the drawer loads.
// The replay archive download uses the same limit.
export const taskDetailInventoryLimit = 80;

export function useTaskDetail() {
  const { t } = useI18n();

  const detailVisible = ref(false);
  const detailTask = ref(null);
  const detailReplication = ref(null);
  const detailLease = ref(null);
  const detailRuns = ref([]);
  const runHistoryLimit = ref(RUN_HISTORY_LIMIT);
  const checkpoint = ref(null);
  const events = ref([]);
  const files = ref([]);
  const replay = ref(null);
  const recovery = ref(null);

  function parseErr(err) {
    return err?.response?.data?.error || err?.message || t("msg.unknownError");
  }

  async function showDetail(taskOrID) {
    try {
      const id = typeof taskOrID === "string" ? taskOrID : taskOrID.id;
      const [task, cp, evs, fs, replaySet, replication, recoveryWindow] = await Promise.all([
        getTask(id),
        getCheckpoint(id),
        listEvents(id, 120),
        listFiles(id, taskDetailInventoryLimit),
        listReplay(id, taskDetailInventoryLimit),
        getReplication(id),
        getRecoveryWindow(id),
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
      replay.value = replaySet || { paths: [] };
      recovery.value = recoveryWindow || { continuous: true, breaks: [] };
      detailVisible.value = true;
    } catch (err) {
      ElMessage.error(parseErr(err));
    }
  }

  return {
    detailVisible,
    detailTask,
    detailReplication,
    detailLease,
    detailRuns,
    runHistoryLimit,
    checkpoint,
    events,
    files,
    replay,
    recovery,
    showDetail,
  };
}
