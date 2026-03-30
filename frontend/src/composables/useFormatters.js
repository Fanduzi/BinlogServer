// input: cluster state, dashboard helpers, i18n, locale
// output: label/tag/format helper functions used in template
// pos: presentation utility composable; no side effects
import { useI18n } from "vue-i18n";

const LEASE_RISK_SECONDS = 45;

export function useFormatters({ cluster, toTimeMs, nowRefMs, currentLocale }) {
  const { t } = useI18n();

  function ownerWorkerLabel(task) {
    const leaseWorker = cluster.leaseByTask[task?.id]?.owner_worker_id;
    return task?.owner_worker_id || leaseWorker || "--";
  }

  function leaseRiskTagType(task, leaseOverride = null) {
    const lease = leaseOverride || cluster.leaseByTask[task?.id];
    const owner = lease?.owner_worker_id || task?.owner_worker_id;
    const epoch = Number(lease?.epoch ?? task?.epoch ?? 0);
    if (!owner || epoch <= 0) return "info";
    const updatedMs = toTimeMs(lease?.updated_at || task?.updated_at);
    if (updatedMs <= 0) return "warning";
    return nowRefMs() - updatedMs > LEASE_RISK_SECONDS * 1000 ? "warning" : "success";
  }

  function leaseRiskLabel(task, leaseOverride = null) {
    const lease = leaseOverride || cluster.leaseByTask[task?.id];
    const owner = lease?.owner_worker_id || task?.owner_worker_id;
    const epoch = Number(lease?.epoch ?? task?.epoch ?? 0);
    if (!owner || epoch <= 0) return "--";
    const updatedMs = toTimeMs(lease?.updated_at || task?.updated_at);
    if (updatedMs <= 0) return t("lease.risk");
    return nowRefMs() - updatedMs > LEASE_RISK_SECONDS * 1000 ? t("lease.risk") : t("lease.normal");
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
    if (!hasProgress || delaySeconds === undefined || delaySeconds === null) return "--";
    return String(delaySeconds);
  }

  function formatTs(ts) {
    if (!ts) return "--";
    const date = new Date(ts);
    if (Number.isNaN(date.getTime())) return "--";
    return date.toLocaleString(currentLocale.value);
  }

  function formatCheckpoint(cp) {
    if (!cp) return t("detail.noCheckpoint");
    const file = cp.file || cp.File || cp.file_name || cp.FileName || cp.binlog_file || cp.BinlogFile || "-";
    const pos = cp.pos ?? cp.Pos ?? cp.position ?? cp.Position ?? cp.binlog_pos ?? cp.BinlogPos ?? 0;
    return `${file}:${pos}`;
  }

  function formatReplicationReason(rep) {
    if (!rep) return "--";
    const reasonMap = {
      NO_PROGRESS: t("replication.noProgress"),
      DELAY_EXCEEDS_THRESHOLD: t("replication.delayExceedsThreshold"),
      RUNNER_ERROR: t("replication.runnerError"),
      TASK_STATE_ERROR: t("replication.taskStateError"),
    };
    const rawErr = rep.last_error || rep.error || rep.err || rep.message || "";
    if (rawErr) {
      const label = reasonMap[rep.reason] || rep.reason || t("replication.runnerError");
      return `${label}: ${rawErr}`;
    }
    if (rep.reason) return reasonMap[rep.reason] || rep.reason;
    if (rep.status === "DELAYED") return t("replication.delayExceedsThreshold");
    if (rep.status === "ABNORMAL") return t("replication.abnormalNoDetail");
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

  return {
    ownerWorkerLabel,
    leaseRiskTagType,
    leaseRiskLabel,
    stateTagType,
    replicationTagType,
    formatDelay,
    formatTs,
    formatCheckpoint,
    formatReplicationReason,
    hasReplicationReason,
    parseErr,
  };
}
