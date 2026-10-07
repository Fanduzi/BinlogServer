// input: mock scenario name plus normalized API request method/path/query/body tuples
// output: deterministic mock API responses including batch task results, numeric-id-ordered dashboard pagination/filter validation, lookup/dashboard SameSourceHost filtering (same accept/reject set as Go ParseIP loopback), single-process overview when the only owner is standalone and workers are empty, independent STARTING counters, per-task resume checkpoints, GET /api/tasks/{id}/replay one path per source index, the same route with stop_datetime returning a UTC point-in-time command, the same route with stop_gtid returning a stop-position command, GET /api/tasks/{id}/replay/archive those basenames, GET /api/tasks/{id}/files/{name} for one inventory basename, and POST adopt of a leftover directory for frontend dev mode and Playwright route interception
// pos: shared frontend mock request handler between api.js and test route adapters
// note: if this file changes, update this header and frontend/src/mocks/README.md.

import { cloneMockValue, getMockScenario } from "./mock-data.js";

const DEFAULT_TIMESTAMP = "2026-03-25T08:00:00Z";
const DEFAULT_TASK_LIMIT = 100;
const MAX_TASK_LIMIT = 500;
const TASK_STATES = new Set([
  "CREATED",
  "STARTING",
  "RUNNING",
  "LEASE_DEGRADED",
  "REBUILDING_FILE",
  "RETRY_BACKOFF",
  "FAILED",
  "STOPPING",
  "STOPPED",
]);

function parseInteger(value) {
  const raw = String(value ?? "");
  if (!/^-?\d+$/.test(raw)) return null;
  const number = Number(raw);
  return Number.isSafeInteger(number) ? number : null;
}

function parseIPv4Octets(text) {
  const parts = String(text).split(".");
  if (parts.length !== 4) return null;
  const octets = [];
  for (const part of parts) {
    if (!/^(0|[1-9]\d{0,2})$/.test(part)) return null;
    const value = Number(part);
    if (value > 255) return null;
    octets.push(value);
  }
  return octets;
}

function parseIPv6HexGroups(side) {
  if (side === "") return [];
  const parts = side.split(":");
  if (parts.some((part) => part === "" || !/^[0-9a-f]{1,4}$/.test(part))) return null;
  return parts.map((part) => Number.parseInt(part, 16));
}

function parseIPv6Bytes(text) {
  if (!text.includes(":")) return null;
  let raw = text;
  let ipv4 = null;
  if (text.includes(".")) {
    const lastColon = text.lastIndexOf(":");
    ipv4 = parseIPv4Octets(text.slice(lastColon + 1));
    if (!ipv4) return null;
    raw = text.slice(0, lastColon);
    if (raw.endsWith(":")) raw += ":";
  }
  const compression = raw.indexOf("::");
  if (compression !== -1 && raw.indexOf("::", compression + 2) !== -1) return null;
  const need = ipv4 ? 6 : 8;
  let groups;
  if (compression === -1) {
    groups = parseIPv6HexGroups(raw);
    if (!groups || groups.length !== need) return null;
  } else {
    const left = parseIPv6HexGroups(raw.slice(0, compression));
    const right = parseIPv6HexGroups(raw.slice(compression + 2));
    if (!left || !right) return null;
    const zeros = need - left.length - right.length;
    if (zeros < 1) return null;
    groups = [...left, ...Array(zeros).fill(0), ...right];
  }
  const bytes = [];
  for (const group of groups) {
    bytes.push((group >> 8) & 0xff, group & 0xff);
  }
  if (ipv4) bytes.push(...ipv4);
  return bytes.length === 16 ? bytes : null;
}

function isLoopbackIPLiteral(text) {
  const v4 = parseIPv4Octets(text);
  if (v4) return v4[0] === 127;
  const v6 = parseIPv6Bytes(text);
  if (!v6) return false;
  const mapped = v6.slice(0, 10).every((byte) => byte === 0) && v6[10] === 0xff && v6[11] === 0xff;
  if (mapped) return v6[12] === 127;
  return v6.slice(0, 15).every((byte) => byte === 0) && v6[15] === 1;
}

function isLoopbackHost(host) {
  let normalized = String(host ?? "")
    .trim()
    .toLowerCase()
    .replace(/\.$/, "");
  if (normalized === "localhost") return true;
  if (normalized.length >= 2 && normalized.startsWith("[") && normalized.endsWith("]")) {
    normalized = normalized.slice(1, -1);
    if (!normalized.includes(":")) return false;
  }
  return isLoopbackIPLiteral(normalized);
}

function sameSourceHost(left, right) {
  if (left === right) return true;
  return isLoopbackHost(left) && isLoopbackHost(right);
}

function compareNumericTaskID(a, b) {
  const left = String(a ?? "");
  const right = String(b ?? "");
  const leftNum = /^[0-9]+$/.test(left) ? Number(left) : null;
  const rightNum = /^[0-9]+$/.test(right) ? Number(right) : null;
  const leftOK = leftNum !== null && Number.isSafeInteger(leftNum);
  const rightOK = rightNum !== null && Number.isSafeInteger(rightNum);
  if (leftOK && rightOK) {
    if (leftNum !== rightNum) return leftNum - rightNum;
    return left.localeCompare(right);
  }
  if (leftOK) return -1;
  if (rightOK) return 1;
  return left.localeCompare(right);
}

function deepClone(value) {
  return JSON.parse(JSON.stringify(value));
}

function sanitizeTask(task) {
  const output = deepClone(task);
  if (output.source) output.source.password = "";
  return output;
}

function isLeftoverDiskTask(task) {
  const source = task?.source || {};
  return (
    String(source.host || "").trim() === "" &&
    String(source.user || "").trim() === "" &&
    String(task?.cluster_key || "").trim() === ""
  );
}

const DISK_DEFAULT_START = { mode: "FILE_POS", file: "mysql-bin.000004", pos: 128 };

function adoptDiskTask(state, id, payload) {
  const row = findTaskRow(state, id);
  if (!row) return ok({ error: "task not found" }, 404);
  if (!isLeftoverDiskTask(row.task)) return ok({ error: "task already has metadata" }, 400);
  const source = payload?.source || null;
  if (!String(payload?.cluster_key || "").trim() || !source?.host || !source?.port || !source?.user || !source?.password) {
    return ok({ error: "source.host/port/user/password is required" }, 400);
  }
  const mode = String(payload.start?.mode || "").trim();
  let start = { ...DISK_DEFAULT_START };
  if (mode === "LATEST") {
    start = { mode: "LATEST" };
  } else if (mode === "FILE_POS") {
    if (!payload.start.file || !Number(payload.start.pos)) return ok({ error: "file and pos are required" }, 400);
    start = { mode: "FILE_POS", file: payload.start.file, pos: Number(payload.start.pos) };
  } else if (mode === "GTID") {
    if (!String(payload.start.gtid_set || "").trim()) return ok({ error: "gtid_set is required" }, 400);
    start = { mode: "GTID", gtid_set: payload.start.gtid_set };
  } else if (mode) {
    return ok({ error: "invalid start mode" }, 400);
  }
  row.task = {
    ...row.task,
    name: payload.name || row.task.name || id,
    cluster_key: payload.cluster_key,
    state: "STOPPED",
    source: {
      host: source.host,
      port: Number(source.port),
      user: source.user,
      flavor: source.flavor || "mysql",
      server_id: Number(source.server_id || 0),
      semi_sync: !!source.semi_sync,
      password: source.password,
    },
    start,
    storage: { retention_days: Number(payload.storage?.retention_days || 7) },
    updated_at: DEFAULT_TIMESTAMP,
  };
  syncTaskSnapshot(state, id);
  return ok(sanitizeTask(row.task));
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
    // standalone is the in-process puller, not a worker, unless the scenario listed it.
    if (!byID.has(owner)) {
      if (owner === "standalone") continue;
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
  const workers = buildWorkers(state);
  const owners = new Set();
  for (const row of state.tasks) {
    const owner = state.leasesByID[row.task.id]?.owner_worker_id || row.task?.owner_worker_id || "";
    if (owner) owners.add(owner);
  }
  const singleProcess = workers.length === 0 && owners.size === 1 && owners.has("standalone");
  return {
    task_count: state.tasks.length,
    worker_count: workers.length,
    running_task_count: state.tasks.filter((row) => row.task?.state === "RUNNING").length,
    leased_task_count: state.tasks.filter((row) => state.leasesByID[row.task.id]?.owner_worker_id).length,
    single_process: singleProcess,
  };
}

function buildLookupResponse(state, query) {
  const host = String(query.get("host") || "").trim();
  const port = Number(query.get("port") || 0);
  const count = state.tasks.filter((row) => {
    const source = row.task?.source || {};
    return sameSourceHost(source.host, host) && Number(source.port) === port;
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
      Object.prototype.hasOwnProperty.call(row, "checkpoint")
        ? row.checkpoint
        : scenario.checkpoint && detailsByID[id].id === scenario.taskDetail?.id
          ? scenario.checkpoint
          : null,
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

function parseTaskQuery(query) {
  const result = {
    host: String(query.get("host") || "").trim(),
    port: null,
    state: null,
    limit: DEFAULT_TASK_LIMIT,
    offset: 0,
  };
  if (query.has("port")) {
    const port = parseInteger(query.get("port"));
    if (port === null || port < 1 || port > 65535) return { error: "invalid port" };
    result.port = port;
  }
  if (query.has("state")) {
    const state = String(query.get("state") || "").trim();
    if (!TASK_STATES.has(state)) return { error: "invalid state" };
    result.state = state;
  }
  if (query.has("limit")) {
    const limit = parseInteger(query.get("limit"));
    if (limit === null || limit <= 0 || limit > MAX_TASK_LIMIT) return { error: "invalid limit" };
    result.limit = limit;
  }
  if (query.has("offset")) {
    const offset = parseInteger(query.get("offset"));
    if (offset === null || offset < 0) return { error: "invalid offset" };
    result.offset = offset;
  }
  return result;
}

function filteredTaskRows(state, taskQuery) {
  return state.tasks
    .filter((row) => {
      const source = row.task?.source || {};
      return (
        (!taskQuery.host || sameSourceHost(source.host, taskQuery.host)) &&
        (taskQuery.port === null || Number(source.port) === taskQuery.port) &&
        (!taskQuery.state || row.task?.state === taskQuery.state)
      );
    })
    .sort((a, b) => compareNumericTaskID(a.task?.id, b.task?.id));
}

function currentDashboard(state, query) {
  const taskQuery = parseTaskQuery(query);
  if (taskQuery.error) return ok({ error: taskQuery.error }, 400);
  const rows = filteredTaskRows(state, taskQuery);
  const end = Math.min(rows.length, taskQuery.offset + taskQuery.limit);
  return ok({
    generated_at: DEFAULT_TIMESTAMP,
    threshold_seconds: 30,
    total: rows.length,
    limit: taskQuery.limit,
    offset: taskQuery.offset,
    summary: buildSummary(rows),
    tasks: deepClone(rows.slice(taskQuery.offset, end)).map((row) => ({
      ...row,
      task: sanitizeTask(row.task),
    })),
    sources: buildSourceSummary(rows),
  });
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

// ponytail: mirrors tasks.SelectReplayFiles / ReplayClient for the dev mock. Go tests lock the rule.
function replaySegmentKey(file) {
  const path = String((file && file.file_path) || "");
  const name = path.split(/[/\\]/).filter(Boolean).pop() || "";
  const mark = ".open.e";
  let source = name;
  let epoch = -1;
  const idx = name.lastIndexOf(mark);
  if (idx > 0) {
    const epochText = name.slice(idx + mark.length);
    if (!/^\d+$/.test(epochText)) return { ok: false, seq: 0, epoch: 0, name };
    source = name.slice(0, idx);
    epoch = Number(epochText);
  }
  const dot = source.lastIndexOf(".");
  if (dot <= 0 || !/^\d+$/.test(source.slice(dot + 1)) || source.slice(0, dot) === "") {
    return { ok: false, seq: 0, epoch: 0, name };
  }
  return { ok: true, seq: Number(source.slice(dot + 1)), epoch, name };
}

function replayLess(a, b) {
  const ak = replaySegmentKey(a);
  const bk = replaySegmentKey(b);
  if (ak.ok && bk.ok && ak.seq !== bk.seq) return ak.seq < bk.seq;
  if (ak.ok !== bk.ok) return ak.ok;
  if (ak.epoch !== bk.epoch) return ak.epoch < bk.epoch;
  return ak.name < bk.name;
}

function windowReplayFiles(files, limit) {
  const sorted = [...files].sort((a, b) => (replayLess(a, b) ? -1 : replayLess(b, a) ? 1 : 0));
  const n = limit > 0 ? limit : 200;
  return sorted.length > n ? sorted.slice(sorted.length - n) : sorted;
}

function selectReplayPaths(files) {
  const paths = [];
  let last = null;
  for (const file of files) {
    const key = replaySegmentKey(file);
    const filePath = String((file && file.file_path) || "").trim();
    if (!key.ok || !filePath) continue;
    if (last && last.seq === key.seq) {
      if (key.epoch >= last.epoch) {
        paths[paths.length - 1] = filePath;
        last = key;
      }
      continue;
    }
    paths.push(filePath);
    last = key;
  }
  return paths;
}

function fileDiskBase(file) {
  const path = file && file.file_path ? String(file.file_path) : "";
  if (!path) return (file && file.file_name) || "";
  const parts = path.split(/[/\\]/);
  return parts[parts.length - 1] || ((file && file.file_name) || "");
}

// Fixed spans for the healthy inventory. Go reads event headers; this mock only
// places those two selected names so the Console drill can call the same query.
const pitrSpanByName = {
  "mysql-bin.000001": ["2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z"],
  "mysql-bin.000002.open.e4": ["2024-01-01T01:00:00Z", "2024-01-02T00:00:00Z"],
};

function parsePITRDatetime(raw) {
  const text = String(raw ?? "").trim();
  if (!text) return null;
  const clock = text.match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})$/);
  if (clock) {
    const year = Number(clock[1]);
    const month = Number(clock[2]);
    const day = Number(clock[3]);
    const hour = Number(clock[4]);
    const minute = Number(clock[5]);
    const second = Number(clock[6]);
    if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) return null;
    const parsed = new Date(Date.UTC(year, month - 1, day, hour, minute, second));
    if (
      parsed.getUTCFullYear() !== year ||
      parsed.getUTCMonth() !== month - 1 ||
      parsed.getUTCDate() !== day
    ) {
      return null;
    }
    return parsed;
  }
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(text)) {
    const parsed = new Date(text);
    if (Number.isNaN(parsed.getTime())) return null;
    return parsed;
  }
  return null;
}

function pitrClock(value) {
  const pad = (n) => String(n).padStart(2, "0");
  return `${value.getUTCFullYear()}-${pad(value.getUTCMonth() + 1)}-${pad(value.getUTCDate())} ${pad(value.getUTCHours())}:${pad(value.getUTCMinutes())}:${pad(value.getUTCSeconds())}`;
}

const mysqlStopGTID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[1-9][0-9]*$/i;
const mariaStopGTID = /^[0-9]+-[0-9]+-[0-9]+$/;
const fixtureStopGTID = "3e11fa47-71ca-11e1-9e33-c80aa9429562:8";
const fixtureStopGTIDAt = Date.parse("2024-01-01T01:20:00Z");

function pitrQuery(query) {
  const stopSet = query.has("stop_datetime");
  const startSet = query.has("start_datetime");
  const gtidSet = query.has("stop_gtid");
  if (gtidSet && stopSet) return { error: "stop_datetime and stop_gtid cannot both be set" };
  if (gtidSet) {
    const raw = String(query.get("stop_gtid") || "").trim();
    const mysql = mysqlStopGTID.test(raw);
    const maria = mariaStopGTID.test(raw);
    if (!raw || (!mysql && !maria)) return { error: "invalid stop_gtid" };
    let start = null;
    if (startSet && String(query.get("start_datetime") || "").trim()) {
      start = parsePITRDatetime(query.get("start_datetime"));
      if (!start) return { error: "invalid start_datetime" };
    }
    return { enabled: true, gtid: true, raw, mysql, start };
  }
  if (!stopSet && !startSet) return { enabled: false };
  if (!stopSet) return { error: "stop_datetime is required" };
  const stop = parsePITRDatetime(query.get("stop_datetime"));
  if (!stop) return { error: "invalid stop_datetime" };
  let start = null;
  if (startSet && String(query.get("start_datetime") || "").trim()) {
    start = parsePITRDatetime(query.get("start_datetime"));
    if (!start) return { error: "invalid start_datetime" };
    if (start.getTime() > stop.getTime()) return { error: "start_datetime is after stop_datetime" };
  }
  return { enabled: true, start, stop };
}

function filterPITRPaths(paths, start, stop) {
  return paths.filter((filePath) => {
    const span = pitrSpanByName[fileDiskBase({ file_path: filePath })];
    if (!span) return false;
    const first = Date.parse(span[0]);
    const last = Date.parse(span[1]);
    if (!(first < stop.getTime())) return false;
    if (start && last < start.getTime()) return false;
    return true;
  });
}

function shellToken(text) {
  const value = String(text);
  if (/[^A-Za-z0-9_./:@+-]/.test(value)) {
    return `'${value.replace(/'/g, `'\\''`)}'`;
  }
  return value;
}

function formatGTIDCommand(client, paths, start, stopPos) {
  if (!paths.length) return "";
  const tokens = [];
  if (start) tokens.push(`--start-datetime=${shellToken(pitrClock(start))}`);
  tokens.push(`--stop-position=${stopPos}`);
  paths.forEach((filePath) => tokens.push(shellToken(filePath)));
  const lines = [];
  if (client) lines.push(`TZ=UTC ${client} \\`);
  tokens.forEach((token, index) => {
    const indent = client || index > 0 ? "  " : "";
    const cont = index === tokens.length - 1 ? "" : " \\";
    lines.push(`${indent}${token}${cont}`);
  });
  return lines.join("\n");
}

function gtidStopResult(task, files, pitr) {
  const flavor = String(task.source?.flavor || "");
  if (flavor.trim().toLowerCase() !== "mysql") {
    return { status: 400, error: "stop_gtid is not supported for this flavor" };
  }
  if (!pitr.mysql) return { status: 400, error: "invalid stop_gtid" };
  if (pitr.raw.toLowerCase() !== fixtureStopGTID) {
    return { status: 400, error: "stop_gtid is not in this task's backed-up range" };
  }
  if (pitr.start && pitr.start.getTime() > fixtureStopGTIDAt) {
    return { status: 400, error: "start_datetime is after stop_gtid" };
  }
  let paths = selectReplayPaths(windowReplayFiles(files, Number.MAX_SAFE_INTEGER));
  if (pitr.start) {
    const startMs = pitr.start.getTime();
    paths = paths.filter((filePath, index) => {
      if (index === paths.length - 1) return true;
      const span = pitrSpanByName[fileDiskBase({ file_path: filePath })];
      if (!span) return false;
      return Date.parse(span[1]) >= startMs;
    });
  }
  const client = replayClient(flavor);
  return {
    flavor,
    client: client.client,
    client_hint: client.client_hint,
    paths,
    command: formatGTIDCommand(client.client, paths, pitr.start, 154),
  };
}

function formatPITRCommand(client, paths, start, stop) {
  if (!paths.length) return "";
  const tokens = [];
  if (start) tokens.push(`--start-datetime=${shellToken(pitrClock(start))}`);
  tokens.push(`--stop-datetime=${shellToken(pitrClock(stop))}`);
  paths.forEach((filePath) => tokens.push(shellToken(filePath)));
  const lines = [];
  if (client) lines.push(`TZ=UTC ${client} \\`);
  tokens.forEach((token, index) => {
    const indent = client || index > 0 ? "  " : "";
    const cont = index === tokens.length - 1 ? "" : " \\";
    lines.push(`${indent}${token}${cont}`);
  });
  return lines.join("\n");
}

function replayClient(flavor) {
  const value = String(flavor || "").trim().toLowerCase();
  if (value === "mysql") return { client: "mysqlbinlog", client_hint: "MySQL mysqlbinlog" };
  if (value === "mariadb") return { client: "mariadb-binlog", client_hint: "mariadb-binlog" };
  return { client: "", client_hint: "" };
}

function replayLimit(query) {
  if (!query.has("limit")) return 200;
  const n = parseInteger(query.get("limit"));
  if (n === null || n < 1) return 200;
  return n;
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

function validateMockCreatePayload(payload) {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return "invalid json";
  if (!String(payload.name || "").trim()) return "invalid name";
  if (!String(payload.cluster_key || "").trim()) return "cluster_key is required";
  const source = payload.source;
  if (!source) return "source.host/port/user/password is required";
  if (!source.host || !source.port || !source.user) return "source.host/port/user/password is required";
  if (!source.password) return "source.password is required";
  if (!Number.isInteger(Number(source.port)) || Number(source.port) < 1 || Number(source.port) > 65535) {
    return "invalid source config";
  }
  return "";
}

function createTaskBatchFromPayload(state, body) {
  const items = body?.items;
  if (!Array.isArray(items)) return ok({ error: "items must be an array", code: "INVALID_REQUEST" }, 400);
  if (items.length === 0) return ok({ error: "items must not be empty", code: "INVALID_REQUEST" }, 400);
  if (items.length > 100) return ok({ error: "items must contain at most 100 items", code: "INVALID_REQUEST" }, 400);

  return ok(items.map((rawItem, index) => {
    const item = cloneMockValue(rawItem);
    const clusterKey = typeof item?.cluster_key === "string" ? item.cluster_key : "";
    const validationError = validateMockCreatePayload(item);
    if (validationError) {
      return {
        index,
        cluster_key: clusterKey,
        error: { error: validationError, code: "INVALID_REQUEST" },
      };
    }
    if (state.tasks.some((row) => row.task.cluster_key === clusterKey)) {
      return {
        index,
        cluster_key: clusterKey,
        error: { error: "cluster_key already exists", code: "INVALID_REQUEST" },
      };
    }
    const task = createTaskRowFromPayload(state, item);
    return { index, cluster_key: task.cluster_key, task: sanitizeTask(task) };
  }));
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
    return currentDashboard(state, query);
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
    const taskQuery = parseTaskQuery(query);
    if (taskQuery.error) return ok({ error: taskQuery.error }, 400);
    const rows = filteredTaskRows(state, taskQuery);
    const end = Math.min(rows.length, taskQuery.offset + taskQuery.limit);
    return ok({
      items: rows.slice(taskQuery.offset, end).map((row) => sanitizeTask(row.task)),
      total: rows.length,
      limit: taskQuery.limit,
      offset: taskQuery.offset,
    });
  }

  if (path === "/api/tasks" && method === "POST") {
    const created = createTaskRowFromPayload(state, cloneMockValue(input.body || {}));
    return ok(sanitizeTask(created), 201);
  }

  if (path === "/api/tasks/batch" && method === "POST") {
    return createTaskBatchFromPayload(state, input.body);
  }

  const taskMatch = path.match(/^\/api\/tasks\/([^/]+)$/);
  if (taskMatch && method === "GET") {
    const id = taskMatch[1];
    return ok(sanitizeTask(state.detailsByID[id] || findTaskRow(state, id)?.task || {}));
  }
  if (taskMatch && method === "PUT") {
    const id = taskMatch[1];
    const row = findTaskRow(state, id);
    if (!row) return ok({ error: "task not found" }, 404);
    if (isLeftoverDiskTask(row.task)) return ok({ error: "on-disk backup has no task metadata" }, 400);
    const updated = updateTaskRow(state, id, cloneMockValue(input.body || {}));
    return updated ? ok(sanitizeTask(updated)) : ok({ error: "task not found" }, 404);
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

  const downloadMatch = path.match(/^\/api\/tasks\/([^/]+)\/files\/([^/]+)$/);
  if (downloadMatch && method === "GET") {
    const id = downloadMatch[1];
    let name = downloadMatch[2];
    try {
      name = decodeURIComponent(name);
    } catch {
      return { status: 400, body: "invalid segment name", contentType: "text/plain" };
    }
    if (!name || name.includes("/") || name.includes("\\") || name.includes("..")) {
      return { status: 400, body: "invalid segment name", contentType: "text/plain" };
    }
    const task = state.detailsByID[id];
    if (!task) return { status: 404, body: "task not found", contentType: "text/plain" };
    const files = state.filesByID[id] || [];
    const known = files.some((file) => fileDiskBase(file) === name);
    if (!known) {
      return { status: 404, body: "segment not found on this process", contentType: "text/plain" };
    }
    return {
      status: 200,
      body: `segment-bytes:${name}`,
      contentType: "application/octet-stream",
      filename: name,
    };
  }

  const replayArchiveMatch = path.match(/^\/api\/tasks\/([^/]+)\/replay\/archive$/);
  if (replayArchiveMatch && method === "GET") {
    const id = replayArchiveMatch[1];
    const pitr = pitrQuery(query);
    if (pitr.error) return { status: 400, body: pitr.error, contentType: "text/plain" };
    const task = state.detailsByID[id];
    if (!task) return { status: 404, body: "task not found", contentType: "text/plain" };
    const files = state.filesByID[id] || [];
    if (pitr.gtid) {
      const gtid = gtidStopResult(task, files, pitr);
      if (gtid.error) return { status: gtid.status, body: gtid.error, contentType: "text/plain" };
      const names = gtid.paths.map((filePath) => fileDiskBase({ file_path: filePath })).filter(Boolean);
      return {
        status: 200,
        body: names.join("\n"),
        contentType: "application/x-tar",
        filename: `task-${id}-replay.tar`,
      };
    }
    let paths = selectReplayPaths(windowReplayFiles(files, pitr.enabled ? Number.MAX_SAFE_INTEGER : replayLimit(query)));
    if (pitr.enabled) paths = filterPITRPaths(paths, pitr.start, pitr.stop);
    const names = paths.map((filePath) => fileDiskBase({ file_path: filePath })).filter(Boolean);
    return {
      status: 200,
      body: names.join("\n"),
      contentType: "application/x-tar",
      filename: `task-${id}-replay.tar`,
    };
  }

  const replayMatch = path.match(/^\/api\/tasks\/([^/]+)\/replay$/);
  if (replayMatch && method === "GET") {
    const id = replayMatch[1];
    const pitr = pitrQuery(query);
    if (pitr.error) return { status: 400, body: pitr.error, contentType: "text/plain" };
    const task = state.detailsByID[id];
    if (!task) {
      if (pitr.enabled) return { status: 404, body: "task not found", contentType: "text/plain" };
      return ok({ error: "task not found" }, 404);
    }
    const files = state.filesByID[id] || [];
    if (pitr.gtid) {
      const gtid = gtidStopResult(task, files, pitr);
      if (gtid.error) return { status: gtid.status, body: gtid.error, contentType: "text/plain" };
      return ok({
        flavor: gtid.flavor,
        client: gtid.client,
        client_hint: gtid.client_hint,
        paths: gtid.paths,
        command: gtid.command,
      });
    }
    let paths = selectReplayPaths(windowReplayFiles(files, pitr.enabled ? Number.MAX_SAFE_INTEGER : replayLimit(query)));
    const flavor = String(task.source?.flavor || "");
    const client = replayClient(flavor);
    if (pitr.enabled) {
      paths = filterPITRPaths(paths, pitr.start, pitr.stop);
      return ok({
        flavor,
        client: client.client,
        client_hint: client.client_hint,
        paths,
        command: formatPITRCommand(client.client, paths, pitr.start, pitr.stop),
      });
    }
    return ok({
      flavor,
      client: client.client,
      client_hint: client.client_hint,
      paths,
    });
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

  const adoptMatch = path.match(/^\/api\/tasks\/([^/]+)\/adopt$/);
  if (adoptMatch && method === "POST") {
    return adoptDiskTask(state, adoptMatch[1], cloneMockValue(input.body || {}));
  }

  const actionMatch = path.match(/^\/api\/tasks\/([^/]+)\/(start|stop)$/);
  if (actionMatch && method === "POST") {
    const id = actionMatch[1];
    const action = actionMatch[2];
    const row = findTaskRow(state, id);
    if (!row) return ok({ error: "task not found" }, 404);
    if (action === "start" && isLeftoverDiskTask(row.task)) {
      return ok({ error: "on-disk backup has no task metadata" }, 400);
    }
    const updated = setTaskRunningState(state, id, action === "start");
    return updated ? ok({ ok: true }) : ok({ error: "task not found" }, 404);
  }

  return ok({ error: `unmocked api request: ${method} ${path}` }, 500);
}
