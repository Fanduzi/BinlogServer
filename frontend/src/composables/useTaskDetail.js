// input: cluster state (leaseByTask), API calls for task data
// output: detail drawer state and showDetail action
// pos: task detail drawer data management
import { ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage } from "element-plus";
import {
  getTask,
  getCheckpoint,
  listEvents,
  listFiles,
  getReplication,
  getTaskLease,
  listTaskRuns,
} from "../api.js";

const RUN_HISTORY_LIMIT = 10;

export function useTaskDetail(cluster) {
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

  function parseErr(err) {
    return err?.response?.data?.error || err?.message || t("msg.unknownError");
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
    showDetail,
  };
}
