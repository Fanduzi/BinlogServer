// input: frontend mock scenario definitions for dashboard, cluster, task detail, and auth states
// output: reusable mock datasets shared by Vite dev mode and Playwright E2E adapters
// pos: shared frontend mock scenario source of truth under the API abstraction layer
// note: if this file changes, update this header and frontend/README.md

const now = "2026-03-25T08:00:00Z";

function buildTask(id, overrides = {}) {
  return {
    id,
    name: `task-${id}`,
    state: "RUNNING",
    cluster_key: `cluster-${id}`,
    owner_worker_id: "worker-a",
    updated_at: now,
    source: {
      host: "127.0.0.1",
      port: 3306,
      semi_sync: true,
      user: "repl",
      flavor: "mysql",
      server_id: 1001,
    },
    start: { mode: "LATEST" },
    storage: { retention_days: 7 },
    ...overrides,
  };
}

function buildReplication(overrides = {}) {
  return {
    status: "NORMAL",
    delay_seconds: 0,
    has_progress: true,
    threshold_seconds: 30,
    last_event_at: now,
    last_event_file: "mysql-bin.000001",
    last_event_pos: 12345,
    reason: "",
    ...overrides,
  };
}

export const mockScenarioNames = [
  "empty",
  "healthy",
  "anomaly",
  "upload-failed",
  "auth-required",
  "cluster-degraded",
  "lease-risk",
  "control-plane-down-worker-running",
];

export const mockScenarios = {
  empty: {
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
    workers: [],
    clusterOverview: {
      task_count: 0,
      worker_count: 0,
      running_task_count: 0,
      leased_task_count: 0,
    },
  },
  healthy: {
    summary: {
      total: 1,
      running: 1,
      retry_backoff: 0,
      stopped: 0,
      failed: 0,
      normal: 1,
      delayed: 0,
      abnormal: 0,
    },
    tasks: [{ task: buildTask("100"), replication: buildReplication() }],
    taskDetail: buildTask("100"),
    replication: buildReplication(),
    checkpoint: { file: "mysql-bin.000001", pos: 12345 },
    lease: { owner_worker_id: "worker-a", epoch: 7, updated_at: now },
    runs: [
      {
        run_id: "run-1",
        worker_id: "worker-a",
        epoch: 7,
        started_at: now,
        ended_at: "",
        end_reason: "",
      },
    ],
    events: [{ sequence: 1, type: "RUNNING", time: now, message: "任务运行正常" }],
    files: [
      {
        file_name: "mysql-bin.000001",
        size_bytes: 2048,
        start_pos: 4,
        end_pos: 12345,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000001",
      },
    ],
    sources: [
      {
        host: "127.0.0.1",
        port: 3306,
        task_count: 1,
        running: 1,
        normal: 1,
        delayed: 0,
        abnormal: 0,
      },
    ],
    workers: [
      {
        worker_id: "worker-a",
        task_count: 1,
        running: 1,
        leased: 1,
        online: true,
        last_seen_at: now,
      },
    ],
    clusterOverview: {
      task_count: 1,
      worker_count: 1,
      running_task_count: 1,
      leased_task_count: 1,
    },
  },
  anomaly: {
    summary: {
      total: 3,
      running: 1,
      retry_backoff: 0,
      stopped: 0,
      failed: 1,
      normal: 1,
      delayed: 1,
      abnormal: 1,
    },
    tasks: [
      {
        task: buildTask("201", { name: "task-abnormal" }),
        replication: buildReplication({
          status: "ABNORMAL",
          reason: "RUNNER_ERROR",
          error: "replication broken",
          delay_seconds: 15,
        }),
      },
      {
        task: buildTask("202", { name: "task-delayed" }),
        replication: buildReplication({
          status: "DELAYED",
          delay_seconds: 91,
          reason: "DELAY_EXCEEDS_THRESHOLD",
        }),
      },
      {
        task: buildTask("203", { name: "task-failed", state: "FAILED" }),
        replication: buildReplication({ status: "IDLE", has_progress: false }),
      },
    ],
    sources: [
      {
        host: "127.0.0.1",
        port: 3306,
        task_count: 3,
        running: 1,
        normal: 1,
        delayed: 1,
        abnormal: 1,
      },
    ],
    workers: [
      {
        worker_id: "worker-a",
        task_count: 3,
        running: 1,
        leased: 1,
        online: true,
        last_seen_at: now,
      },
    ],
    clusterOverview: {
      task_count: 3,
      worker_count: 1,
      running_task_count: 1,
      leased_task_count: 1,
    },
  },
  "upload-failed": {
    summary: {
      total: 1,
      running: 1,
      retry_backoff: 0,
      stopped: 0,
      failed: 0,
      normal: 0,
      delayed: 0,
      abnormal: 1,
    },
    tasks: [
      {
        task: buildTask("301", { name: "task-upload-failed", owner_worker_id: "worker-b" }),
        replication: buildReplication({
          status: "ABNORMAL",
          reason: "RUNNER_ERROR",
          error: "upload failed",
        }),
      },
    ],
    taskDetail: buildTask("301", {
      name: "task-upload-failed",
      owner_worker_id: "worker-b",
    }),
    replication: buildReplication({
      status: "ABNORMAL",
      reason: "RUNNER_ERROR",
      error: "upload failed",
    }),
    checkpoint: { file: "mysql-bin.000020", pos: 76543 },
    lease: { owner_worker_id: "worker-b", epoch: 11, updated_at: now },
    runs: [
      {
        run_id: "run-upload-1",
        worker_id: "worker-b",
        epoch: 11,
        started_at: now,
        ended_at: "",
        end_reason: "",
      },
    ],
    events: [
      { sequence: 1, type: "UPLOAD_FAILED", time: now, message: "文件上传失败，等待重试" },
    ],
    filesBeforeRetry: [
      {
        file_name: "mysql-bin.000020",
        size_bytes: 4096,
        start_pos: 4,
        end_pos: 76543,
        upload_state: "UPLOAD_FAILED",
        object_key: "bucket/path/mysql-bin.000020",
      },
      {
        file_name: "mysql-bin.000021",
        size_bytes: 1024,
        start_pos: 76544,
        end_pos: 80000,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000021",
      },
    ],
    filesAfterRetry: [
      {
        file_name: "mysql-bin.000020",
        size_bytes: 4096,
        start_pos: 4,
        end_pos: 76543,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000020",
      },
      {
        file_name: "mysql-bin.000021",
        size_bytes: 1024,
        start_pos: 76544,
        end_pos: 80000,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000021",
      },
    ],
    sources: [
      {
        host: "127.0.0.1",
        port: 3307,
        task_count: 1,
        running: 1,
        normal: 0,
        delayed: 0,
        abnormal: 1,
      },
    ],
    workers: [
      {
        worker_id: "worker-b",
        task_count: 1,
        running: 1,
        leased: 1,
        online: true,
        last_seen_at: now,
      },
    ],
    clusterOverview: {
      task_count: 1,
      worker_count: 1,
      running_task_count: 1,
      leased_task_count: 1,
    },
  },
  "cluster-degraded": {
    summary: {
      total: 3,
      running: 2,
      retry_backoff: 0,
      stopped: 0,
      failed: 0,
      normal: 1,
      delayed: 1,
      abnormal: 1,
    },
    tasks: [
      {
        task: buildTask("401", { name: "task-primary-healthy", owner_worker_id: "worker-a" }),
        replication: buildReplication(),
      },
      {
        task: buildTask("402", { name: "task-lagging", owner_worker_id: "worker-b" }),
        replication: buildReplication({
          status: "DELAYED",
          delay_seconds: 155,
          reason: "DELAY_EXCEEDS_THRESHOLD",
        }),
      },
      {
        task: buildTask("403", { name: "task-broken", owner_worker_id: "worker-b" }),
        replication: buildReplication({
          status: "ABNORMAL",
          delay_seconds: 17,
          reason: "RUNNER_ERROR",
          error: "worker disconnected",
        }),
      },
    ],
    sources: [
      { host: "127.0.0.1", port: 3306, task_count: 2, running: 1, normal: 1, delayed: 1, abnormal: 0 },
      { host: "127.0.0.1", port: 3307, task_count: 1, running: 1, normal: 0, delayed: 0, abnormal: 1 },
    ],
    workers: [
      { worker_id: "worker-a", task_count: 1, running: 1, leased: 1, online: true, last_seen_at: now },
      { worker_id: "worker-b", task_count: 2, running: 1, leased: 2, online: false, last_seen_at: "2026-03-24T08:00:00Z" },
    ],
    clusterOverview: {
      task_count: 3,
      worker_count: 2,
      running_task_count: 2,
      leased_task_count: 3,
    },
  },
  "lease-risk": {
    summary: {
      total: 1,
      running: 1,
      retry_backoff: 0,
      stopped: 0,
      failed: 0,
      normal: 1,
      delayed: 0,
      abnormal: 0,
    },
    tasks: [
      {
        task: buildTask("501", {
          name: "task-stale-lease",
          owner_worker_id: "worker-stale",
          updated_at: "2026-03-20T08:00:00Z",
        }),
        replication: buildReplication(),
      },
    ],
    taskDetail: buildTask("501", {
      name: "task-stale-lease",
      owner_worker_id: "worker-stale",
      updated_at: "2026-03-20T08:00:00Z",
    }),
    replication: buildReplication(),
    checkpoint: { file: "mysql-bin.000099", pos: 90001 },
    lease: {
      owner_worker_id: "worker-stale",
      epoch: 4,
      updated_at: "2026-03-20T08:00:00Z",
    },
    runs: [
      {
        run_id: "run-stale-1",
        worker_id: "worker-stale",
        epoch: 4,
        started_at: "2026-03-20T07:50:00Z",
        ended_at: "",
        end_reason: "",
      },
    ],
    events: [
      { sequence: 1, type: "RUNNING", time: now, message: "任务仍运行，但 lease 已过旧" },
    ],
    files: [
      {
        file_name: "mysql-bin.000099",
        size_bytes: 2048,
        start_pos: 4,
        end_pos: 90001,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000099",
      },
    ],
    sources: [
      { host: "127.0.0.1", port: 3310, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
    ],
    workers: [
      {
        worker_id: "worker-stale",
        task_count: 1,
        running: 1,
        leased: 1,
        online: false,
        last_seen_at: "2026-03-20T08:00:00Z",
      },
    ],
    clusterOverview: {
      task_count: 1,
      worker_count: 1,
      running_task_count: 1,
      leased_task_count: 1,
    },
  },
  "control-plane-down-worker-running": {
    summary: {
      total: 2,
      running: 2,
      retry_backoff: 0,
      stopped: 0,
      failed: 0,
      normal: 2,
      delayed: 0,
      abnormal: 0,
    },
    tasks: [
      {
        task: buildTask("601", {
          name: "task-continued-during-cp-outage",
          owner_worker_id: "worker-hot-standby",
          updated_at: "2026-03-26T08:20:00Z",
          source: { host: "prod-mysql-order-01.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 20011 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 0, last_event_file: "mysql-bin.000188", last_event_pos: 92001 }),
      },
      {
        task: buildTask("602", {
          name: "task-order-replay-backlog",
          owner_worker_id: "worker-primary",
          updated_at: "2026-03-26T08:18:00Z",
          source: { host: "prod-mysql-order-02.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 20012 },
        }),
        replication: buildReplication({ status: "DELAYED", delay_seconds: 88, last_event_file: "mysql-bin.000522", last_event_pos: 68211, reason: "DELAY_EXCEEDS_THRESHOLD" }),
      },
      {
        task: buildTask("603", {
          name: "task-user-profile-stream",
          owner_worker_id: "worker-sync-a",
          updated_at: "2026-03-26T08:19:00Z",
          source: { host: "prod-mysql-user-01.internal", port: 3306, semi_sync: false, user: "repl", flavor: "mysql", server_id: 21001 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 2, last_event_file: "mysql-bin.000311", last_event_pos: 40121 }),
      },
      {
        task: buildTask("604", {
          name: "task-payment-ledger",
          owner_worker_id: "worker-sync-b",
          updated_at: "2026-03-26T08:16:00Z",
          source: { host: "prod-mysql-payment-01.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 22001 },
        }),
        replication: buildReplication({ status: "ABNORMAL", delay_seconds: 15, last_event_file: "mysql-bin.000921", last_event_pos: 17231, reason: "RUNNER_ERROR", error: "stream broken" }),
      },
      {
        task: buildTask("605", {
          name: "task-inventory-sync",
          owner_worker_id: "worker-sync-a",
          updated_at: "2026-03-26T08:18:30Z",
          source: { host: "prod-mysql-inventory-01.internal", port: 3306, semi_sync: false, user: "repl", flavor: "mysql", server_id: 23001 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 1, last_event_file: "mysql-bin.000073", last_event_pos: 93211 }),
      },
      {
        task: buildTask("606", {
          name: "task-analytics-wal",
          owner_worker_id: "worker-archive",
          updated_at: "2026-03-26T08:17:40Z",
          source: { host: "prod-aurora-analytics.cluster.local", port: 3306, semi_sync: false, user: "repl", flavor: "mysql", server_id: 24001 },
        }),
        replication: buildReplication({ status: "DELAYED", delay_seconds: 137, last_event_file: "mysql-bin.001201", last_event_pos: 19444, reason: "DELAY_EXCEEDS_THRESHOLD" }),
      },
      {
        task: buildTask("607", {
          name: "task-order-history",
          owner_worker_id: "worker-hot-standby",
          updated_at: "2026-03-26T08:19:50Z",
          source: { host: "prod-mysql-order-03.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 20013 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 0, last_event_file: "mysql-bin.000201", last_event_pos: 21111 }),
      },
      {
        task: buildTask("608", {
          name: "task-payment-events",
          owner_worker_id: "worker-sync-b",
          updated_at: "2026-03-26T08:19:10Z",
          source: { host: "prod-mysql-payment-02.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 22002 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 4, last_event_file: "mysql-bin.000411", last_event_pos: 32991 }),
      },
      {
        task: buildTask("609", {
          name: "task-user-growth",
          owner_worker_id: "worker-primary",
          updated_at: "2026-03-26T08:15:50Z",
          source: { host: "prod-mysql-user-02.internal", port: 3306, semi_sync: false, user: "repl", flavor: "mysql", server_id: 21002 },
        }),
        replication: buildReplication({ status: "ABNORMAL", delay_seconds: 12, last_event_file: "mysql-bin.000119", last_event_pos: 8122, reason: "RUNNER_ERROR", error: "network reset" }),
      },
      {
        task: buildTask("610", {
          name: "task-audit-archive",
          owner_worker_id: "worker-archive",
          updated_at: "2026-03-26T08:13:40Z",
          source: { host: "prod-mysql-audit-01.internal", port: 3306, semi_sync: false, user: "repl", flavor: "mysql", server_id: 25001 },
        }),
        replication: buildReplication({ status: "NORMAL", delay_seconds: 6, last_event_file: "mysql-bin.000044", last_event_pos: 69211 }),
      },
      {
        task: buildTask("611", {
          name: "task-order-compensate",
          owner_worker_id: "worker-sync-a",
          updated_at: "2026-03-26T08:10:40Z",
          state: "RETRY_BACKOFF",
          source: { host: "prod-mysql-order-04.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 20014 },
        }),
        replication: buildReplication({ status: "IDLE", delay_seconds: 0, has_progress: false, last_event_file: "", last_event_pos: 0, reason: "TASK_RETRY_BACKOFF" }),
      },
      {
        task: buildTask("612", {
          name: "task-settlement-rebuild",
          owner_worker_id: "worker-hot-standby",
          updated_at: "2026-03-26T08:12:00Z",
          state: "STOPPED",
          source: { host: "prod-mysql-settlement-01.internal", port: 3306, semi_sync: true, user: "repl", flavor: "mysql", server_id: 26001 },
        }),
        replication: buildReplication({ status: "IDLE", delay_seconds: 0, has_progress: false, last_event_file: "", last_event_pos: 0, reason: "TASK_STOPPED" }),
      },
    ],
    taskDetail: buildTask("601", {
      name: "task-continued-during-cp-outage",
      owner_worker_id: "worker-hot-standby",
      updated_at: "2026-03-26T08:20:00Z",
    }),
    replication: buildReplication({
      status: "NORMAL",
      delay_seconds: 0,
      last_event_file: "mysql-bin.000188",
      last_event_pos: 92001,
    }),
    checkpoint: { file: "mysql-bin.000188", pos: 92001 },
    lease: {
      owner_worker_id: "worker-hot-standby",
      epoch: 12,
      updated_at: "2026-03-26T08:20:00Z",
    },
    runs: [
      {
        run_id: "run-cp-down-1",
        worker_id: "worker-hot-standby",
        epoch: 12,
        started_at: "2026-03-26T08:00:00Z",
        ended_at: "",
        end_reason: "",
      },
    ],
    events: [
      { sequence: 1, type: "RUNNING", time: "2026-03-26T08:00:00Z", message: "任务持续运行" },
      {
        sequence: 2,
        type: "CONTROL_PLANE_RECOVERED",
        time: "2026-03-26T08:20:00Z",
        message: "控制面恢复后任务上下文已同步",
      },
    ],
    files: [
      {
        file_name: "mysql-bin.000188",
        size_bytes: 8192,
        start_pos: 4,
        end_pos: 92001,
        upload_state: "UPLOADED",
        object_key: "bucket/path/mysql-bin.000188",
      },
    ],
    sources: [
      { host: "prod-mysql-order-01.internal", port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-order-02.internal", port: 3306, task_count: 1, running: 1, normal: 0, delayed: 1, abnormal: 0 },
      { host: "prod-mysql-user-01.internal", port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-payment-01.internal", port: 3306, task_count: 1, running: 1, normal: 0, delayed: 0, abnormal: 1 },
      { host: "prod-aurora-analytics.cluster.local", port: 3306, task_count: 1, running: 1, normal: 0, delayed: 1, abnormal: 0 },
      { host: "prod-mysql-order-03.internal", port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-payment-02.internal", port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-user-02.internal", port: 3306, task_count: 1, running: 1, normal: 0, delayed: 0, abnormal: 1 },
      { host: "prod-mysql-audit-01.internal", port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-order-04.internal", port: 3306, task_count: 1, running: 0, normal: 0, delayed: 0, abnormal: 0 },
      { host: "prod-mysql-settlement-01.internal", port: 3306, task_count: 1, running: 0, normal: 0, delayed: 0, abnormal: 0 },
    ],
    workers: [
      {
        worker_id: "worker-hot-standby",
        task_count: 3,
        running: 2,
        leased: 3,
        online: true,
        last_seen_at: "2026-03-26T08:20:00Z",
      },
      {
        worker_id: "worker-primary",
        task_count: 2,
        running: 2,
        leased: 2,
        online: false,
        last_seen_at: "2026-03-26T07:58:00Z",
      },
      {
        worker_id: "worker-sync-a",
        task_count: 3,
        running: 2,
        leased: 3,
        online: true,
        last_seen_at: "2026-03-26T08:19:00Z",
      },
      {
        worker_id: "worker-sync-b",
        task_count: 2,
        running: 2,
        leased: 2,
        online: true,
        last_seen_at: "2026-03-26T08:19:10Z",
      },
      {
        worker_id: "worker-archive",
        task_count: 2,
        running: 2,
        leased: 2,
        online: true,
        last_seen_at: "2026-03-26T08:17:40Z",
      },
    ],
    clusterOverview: {
      task_count: 12,
      worker_count: 5,
      running_task_count: 10,
      leased_task_count: 12,
    },
  },
};

export function cloneMockValue(value) {
  return JSON.parse(JSON.stringify(value));
}

export function getMockScenario(name = "healthy") {
  return mockScenarios[name] || mockScenarios.healthy;
}
