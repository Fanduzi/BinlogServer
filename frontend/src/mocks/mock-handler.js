// input: mock scenario name plus normalized API request method/path/query/body tuples
// output: deterministic mock API responses including independent STARTING counters for frontend dev mode and Playwright route interception
// pos: shared frontend mock request handler between api.js and test route adapters
// note: if this file changes, update this header and frontend/README.md

import { cloneMockValue, getMockScenario } from "./mock-data.js";

const DEFAULT_TIMESTAMP = "2026-03-25T08:00:00Z";

function deepClone(value) {
  return JSON.parse(JSON.stringify(value));
}

function normalizeScenarioName(name) {
  return getMockScenario(name) ? name : "healthy";
}

function buildDefaultTask(id, overrides = {}) {
  return {
    id,
    name: `task-${id}`,
    state: "CREATED",
    cluster_key: `cluster-${id}`,
    owner_worker_id: "",
    updated_at: DEFAULT_TIMESTAMP,
    source: {
      host: "127.0.0.1",
      port: 3306,
      semi_sync: false,
      user: "repl",
      flavor: "mysql",
      server_id: 300001,
    },
    start: { mode: "LATEST" },
    storage: { retention_days: 7 },
    ...overrides,
  };
}

function buildDefaultReplication(overrides = {}) {
  return {
    status: "IDLE",
    delay_seconds: 0,
    has_progress: false,
    threshold_seconds: 30,
    last_event_at: DEFAULT_TIMESTAMP,
    last_event_file: "",
    last_event_pos: 0,
    reason: "",
    ...overrides,
  };
}

function buildDefaultLease(task) {
  return {
    owner_worker_id: task.owner_worker_id || "",
    epoch: task.owner_worker_id ? 1 : 0,
    updated_at: task.updated_at || DEFAULT_TIMESTAMP,
  };
}

function buildSourceSummary(rows) {
  const grouped = new Map();
  for (const row of rows) {
    const task = row.task || {};
    const replication = row.replication || {};
    const key = `${task.source?.host || ""}:${task.source?.port || ""}`;
    if (!grouped.has(key)) {
      grouped.set(key, {
        host: task.source?.host || "",
        port: task.source?.port || 0,
        task_count: 0,
        starting: 0,
        running: 0,
        normal: 0,
        delayed: 0,
        abnormal: 0,
      });
    }
    const source = grouped.get(key);
    source.task_count += 1;
    if (task.state === "STARTING") source.starting += 1;
    if (task.state === "RUNNING") source.running += 1;
    if (replication.status === "NORMAL") source.normal += 1;
    if (replication.status === "DELAYED") source.delayed += 1;
    if (replication.status === "ABNORMAL") source.abnormal += 1;
  }
  return Array.from(grouped.values()).filter((item) => item.host);
}

function buildSummary(rows) {
  return {
    total: rows.length,
    starting: rows.filter((row) => row.task?.state === "STARTING").length,
    running: rows.filter((row) => row.task?.state === "RUNNING").length,
    retry_backoff: rows.filter((row) => row.task?.state === "RETRY_BACKOFF").length,
    stopped: rows.filter((row) => row.task?.state === "STOPPED").length,
    failed: rows.filter((row) => row.task?.state === "FAILED").length,
    normal: rows.filter((row) => row.replication?.status === "NORMAL").length,
    delayed: rows.filter((row) => row.replication?.status === "DELAYED").length,
    abnormal: rows.filter((row) => row.replication?.status === "ABNORMAL").length,
  };
}

function buildWorkers(state) {
  const baseWorkers = deepClone(state.workers);
  const byID = new Map(baseWorkers.map((worker) => [worker.worker_id, worker]));

  for (const row of state.tasks) {
    const lease = state.leasesByID[row.task.id];
    const owner = lease?.owner_worker_id || row.task.owner_worker_id;
    if (!owner) continue;
    if (!byID.has(owner)) {
      byID.set(owner, {
        worker_id: owner,
        task_count: 0,
        running: 0,
        leased: 0,
        online: true,
        last_seen_at: DEFAULT_TIMESTAMP,
      });
    }
  }

  for (const worker of byID.values()) {
    worker.task_count = 0;
    worker.running = 0;
    worker.leased = 0;
  }

  for (const row of state.tasks) {
    const lease = state.leasesByID[row.task.id];
    const owner = lease?.owner_worker_id || row.task.owner_worker_id;
    if (!owner || !byID.has(owner)) continue;
    const worker = byID.get(owner);
    worker.task_count += 1;
    if (row.task.state === "RUNNING") worker.running += 1;
    if (lease?.owner_worker_id) worker.leased += 1;
  }

  return Array.from(byID.values());
}

function buildClusterOverview(state) {
  return {
    task_count: state.tasks.length,
    worker_count: buildWorkers(state).length,
    running_task_count: state.tasks.filter((row) => row.task?.state === "RUNNING").length,
    leased_task_count: state.tasks.filter((row) => state.leasesByID[row.task.id]?.owner_worker_id).length,
  };
}

function buildLookupResponse(state, query) {
  const host = String(query.get("host") || "").trim();
  const port = Number(query.get("port") || 0);
  const count = state.tasks.filter((row) => {
    const source = row.task?.source || {};
    return source.host === host && Number(source.port) === port;
  }).length;
  return {
    exists: count > 0,
    count,
  };
}

function createInitialState(scenarioName) {
  const effectiveScenario = scenarioName === "auth-required" ? "healthy" : scenarioName;
  const scenario = getMockScenario(effectiveScenario);
  const tasks = deepClone(scenario.tasks || []);
  const detailsByID = {};
  const replicationsByID = {};
  const checkpointsByID = {};
  const leasesByID = {};
  const runsByID = {};
  const eventsByID = {};
  const filesByID = {};

  for (const row of tasks) {
    const id = row.task.id;
    detailsByID[id] = deepClone(
      scenario.taskDetail && scenario.taskDetail.id === id ? scenario.taskDetail : row.task,
    );
    replicationsByID[id] = deepClone(
      scenario.replication && detailsByID[id].id === scenario.taskDetail?.id
        ? scenario.replication
        : row.replication,
    );
    checkpointsByID[id] = deepClone(
      scenario.checkpoint && detailsByID[id].id === scenario.taskDetail?.id ? scenario.checkpoint : null,
    );
    leasesByID[id] = deepClone(
      scenario.lease && detailsByID[id].id === scenario.taskDetail?.id
        ? scenario.lease
        : buildDefaultLease(row.task),
    );
    runsByID[id] = deepClone(
      scenario.runs && detailsByID[id].id === scenario.taskDetail?.id ? scenario.runs : [],
    );
    eventsByID[id] = deepClone(
      scenario.events && detailsByID[id].id === scenario.taskDetail?.id ? scenario.events : [],
    );
    filesByID[id] = deepClone(
      effectiveScenario === "upload-failed" && id === "301"
        ? scenario.filesBeforeRetry
        : scenario.files || [],
    );
  }

  return {
    scenarioName,
    nextID: tasks.reduce((max, row) => Math.max(max, Number(row.task.id) || 0), 0) + 1,
    retryDone: false,
    tasks,
    workers: deepClone(scenario.workers || []),
    detailsByID,
    replicationsByID,
    checkpointsByID,
    leasesByID,
    runsByID,
    eventsByID,
    filesByID,
  };
}

function currentDashboard(state) {
  return {
    generated_at: DEFAULT_TIMESTAMP,
    threshold_seconds: 30,
    summary: buildSummary(state.tasks),
    tasks: deepClone(state.tasks),
    sources: buildSourceSummary(state.tasks),
  };
}

function currentWorkers(state) {
  return buildWorkers(state);
}

function currentOverview(state) {
  return buildClusterOverview(state);
}

function findTaskRow(state, id) {
  return state.tasks.find((row) => String(row.task.id) === String(id)) || null;
}

function syncTaskSnapshot(state, id) {
  const row = findTaskRow(state, id);
  if (!row) return;
  state.detailsByID[id] = deepClone(row.task);
}

function createTaskRowFromPayload(state, payload) {
  const id = String(state.nextID);
  state.nextID += 1;
  const task = buildDefaultTask(id, {
    name: payload.name || `task-${id}`,
    cluster_key: payload.cluster_key || `cluster-${id}`,
    owner_worker_id: "",
    source: {
      ...buildDefaultTask(id).source,
      ...(payload.source || {}),
    },
    start: {
      mode: payload.start?.mode || "LATEST",
      ...(payload.start || {}),
    },
    storage: {
      retention_days: Number(payload.storage?.retention_days || 7),
    },
  });
  const row = {
    task,
    replication: buildDefaultReplication(),
  };
  state.tasks.unshift(row);
  state.detailsByID[id] = deepClone(task);
  state.replicationsByID[id] = deepClone(row.replication);
  state.checkpointsByID[id] = null;
  state.leasesByID[id] = buildDefaultLease(task);
  state.runsByID[id] = [];
  state.eventsByID[id] = [];
  state.filesByID[id] = [];
  return task;
}

function updateTaskRow(state, id, payload) {
  const row = findTaskRow(state, id);
  if (!row) return null;
  row.task = {
    ...row.task,
    ...payload,
    source: {
      ...row.task.source,
      ...(payload.source || {}),
    },
    start: {
      ...row.task.start,
      ...(payload.start || {}),
    },
    storage: {
      ...row.task.storage,
      ...(payload.storage || {}),
    },
    updated_at: DEFAULT_TIMESTAMP,
  };
  syncTaskSnapshot(state, id);
  return deepClone(row.task);
}

function setTaskRunningState(state, id, isRunning) {
  const row = findTaskRow(state, id);
  if (!row) return false;
  row.task.state = isRunning ? "RUNNING" : "STOPPED";
  row.task.updated_at = DEFAULT_TIMESTAMP;
  row.replication = buildDefaultReplication({
    status: isRunning ? "NORMAL" : "IDLE",
    has_progress: isRunning,
    last_event_file: isRunning ? "mysql-bin.000001" : "",
    last_event_pos: isRunning ? 12345 : 0,
  });
  if (isRunning) {
    const currentLease = state.leasesByID[id] || buildDefaultLease(row.task);
    const owner =
      currentLease.owner_worker_id ||
      row.task.owner_worker_id ||
      currentWorkers(state)[0]?.worker_id ||
      "worker-a";
    state.leasesByID[id] = {
      owner_worker_id: owner,
      epoch: Number(currentLease.epoch || 0) + 1,
      updated_at: DEFAULT_TIMESTAMP,
    };
    row.task.owner_worker_id = owner;
  } else {
    state.leasesByID[id] = {
      owner_worker_id: "",
      epoch: Number(state.leasesByID[id]?.epoch || 0),
      updated_at: DEFAULT_TIMESTAMP,
    };
    row.task.owner_worker_id = "";
  }
  state.replicationsByID[id] = deepClone(row.replication);
  syncTaskSnapshot(state, id);
  return true;
}

function deleteTaskRow(state, id) {
  const index = state.tasks.findIndex((row) => String(row.task.id) === String(id));
  if (index === -1) return false;
  state.tasks.splice(index, 1);
  delete state.detailsByID[id];
  delete state.replicationsByID[id];
  delete state.checkpointsByID[id];
  delete state.leasesByID[id];
  delete state.runsByID[id];
  delete state.eventsByID[id];
  delete state.filesByID[id];
  return true;
}

function handleAuthRequiredScenario(scenarioName, method, path) {
  return (
    scenarioName === "auth-required" &&
    method === "GET" &&
    ["/api/dashboard", "/api/cluster/overview", "/api/workers"].includes(path)
  );
}

function ok(body, status = 200) {
  return { status, body };
}

export function createMockSession(options = {}) {
  const scenario = normalizeScenarioName(options.scenario || "healthy");
  const state = createInitialState(scenario);
  return {
    scenario,
    request(input) {
      return handleMockRequest({
        ...input,
        scenario,
        state,
        onRetryUpload: options.onRetryUpload,
      });
    },
  };
}

export function handleMockRequest(input) {
  const method = String(input.method || "GET").toUpperCase();
  const path = input.path || "/";
  const query =
    input.query instanceof URLSearchParams
      ? input.query
      : new URLSearchParams(input.query || {});
  const scenario = normalizeScenarioName(input.scenario || "healthy");
  const state = input.state || createInitialState(scenario);

  if (handleAuthRequiredScenario(scenario, method, path)) {
    return ok({ error: "unauthorized" }, 401);
  }

  if (path === "/api/dashboard" && method === "GET") {
    const response = currentDashboard(state);
    const host = String(query.get("host") || "").trim();
    const port = Number(query.get("port") || 0);
    if (!host || !port) return ok(response);
    const filteredRows = response.tasks.filter((row) => {
      const source = row.task?.source || {};
      return source.host === host && Number(source.port) === port;
    });
    return ok({
      ...response,
      summary: buildSummary(filteredRows),
      tasks: filteredRows,
      sources: buildSourceSummary(filteredRows),
    });
  }

  if (path === "/api/cluster/overview" && method === "GET") {
    return ok(currentOverview(state));
  }

  if (path === "/api/workers" && method === "GET") {
    return ok(currentWorkers(state));
  }

  if (path === "/api/sources/lookup" && method === "GET") {
    return ok(buildLookupResponse(state, query));
  }

  if (path === "/api/tasks" && method === "GET") {
    return ok(deepClone(state.tasks));
  }

  if (path === "/api/tasks" && method === "POST") {
    const created = createTaskRowFromPayload(state, cloneMockValue(input.body || {}));
    return ok(created, 201);
  }

  const taskMatch = path.match(/^\/api\/tasks\/([^/]+)$/);
  if (taskMatch && method === "GET") {
    const id = taskMatch[1];
    return ok(deepClone(state.detailsByID[id] || findTaskRow(state, id)?.task || {}));
  }
  if (taskMatch && method === "PUT") {
    const id = taskMatch[1];
    const updated = updateTaskRow(state, id, cloneMockValue(input.body || {}));
    return updated ? ok(updated) : ok({ error: "task not found" }, 404);
  }
  if (taskMatch && method === "DELETE") {
    const id = taskMatch[1];
    return deleteTaskRow(state, id) ? { status: 204, body: "" } : ok({ error: "task not found" }, 404);
  }

  const checkpointMatch = path.match(/^\/api\/tasks\/([^/]+)\/checkpoint$/);
  if (checkpointMatch && method === "GET") {
    return ok(deepClone(state.checkpointsByID[checkpointMatch[1]] ?? null));
  }

  const replicationMatch = path.match(/^\/api\/tasks\/([^/]+)\/replication$/);
  if (replicationMatch && method === "GET") {
    return ok(deepClone(state.replicationsByID[replicationMatch[1]] ?? null));
  }

  const leaseMatch = path.match(/^\/api\/tasks\/([^/]+)\/lease$/);
  if (leaseMatch && method === "GET") {
    return ok(deepClone(state.leasesByID[leaseMatch[1]] ?? null));
  }

  const runsMatch = path.match(/^\/api\/tasks\/([^/]+)\/runs$/);
  if (runsMatch && method === "GET") {
    return ok(deepClone(state.runsByID[runsMatch[1]] || []));
  }

  const eventsMatch = path.match(/^\/api\/tasks\/([^/]+)\/events$/);
  if (eventsMatch && method === "GET") {
    return ok(deepClone(state.eventsByID[eventsMatch[1]] || []));
  }

  const filesMatch = path.match(/^\/api\/tasks\/([^/]+)\/files$/);
  if (filesMatch && method === "GET") {
    return ok(deepClone(state.filesByID[filesMatch[1]] || []));
  }

  const retryUploadMatch = path.match(/^\/api\/tasks\/([^/]+)\/files\/retry-upload$/);
  if (retryUploadMatch && method === "POST") {
    const id = retryUploadMatch[1];
    const uploadScenario = getMockScenario("upload-failed");
    if (scenario === "upload-failed" && String(id) === "301") {
      state.retryDone = true;
      state.filesByID[id] = deepClone(uploadScenario.filesAfterRetry);
      if (typeof input.onRetryUpload === "function") input.onRetryUpload();
      return ok({ retried: 1, failed: 0, skipped: 0 });
    }
    return ok({ retried: 0, failed: 0, skipped: 0 });
  }

  const actionMatch = path.match(/^\/api\/tasks\/([^/]+)\/(start|stop)$/);
  if (actionMatch && method === "POST") {
    const id = actionMatch[1];
    const action = actionMatch[2];
    const updated = setTaskRunningState(state, id, action === "start");
    return updated ? ok({ ok: true }) : ok({ error: "task not found" }, 404);
  }

  return ok({ error: `unmocked api request: ${method} ${path}` }, 500);
}
