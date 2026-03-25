export type MockScenario = 'empty' | 'healthy' | 'anomaly' | 'upload-failed' | 'auth-required'

export interface MockTaskRow {
  task: any
  replication: any
}

const now = '2026-03-25T08:00:00Z'

function buildTask(id: string, overrides: Record<string, any> = {}) {
  return {
    id,
    name: `task-${id}`,
    state: 'RUNNING',
    cluster_key: `cluster-${id}`,
    owner_worker_id: 'worker-a',
    updated_at: now,
    source: {
      host: '127.0.0.1',
      port: 3306,
      semi_sync: true,
      user: 'repl',
      flavor: 'mysql',
      server_id: 1001,
    },
    start: { mode: 'LATEST' },
    storage: { retention_days: 7 },
    ...overrides,
  }
}

function buildReplication(overrides: Record<string, any> = {}) {
  return {
    status: 'NORMAL',
    delay_seconds: 0,
    has_progress: true,
    threshold_seconds: 30,
    last_event_at: now,
    last_event_file: 'mysql-bin.000001',
    last_event_pos: 12345,
    reason: '',
    ...overrides,
  }
}

export const scenarios = {
  empty: {
    summary: { total: 0, running: 0, retry_backoff: 0, stopped: 0, failed: 0, normal: 0, delayed: 0, abnormal: 0 },
    tasks: [] as MockTaskRow[],
    sources: [],
    workers: [],
    clusterOverview: { task_count: 0, worker_count: 0, running_task_count: 0, leased_task_count: 0 },
  },
  healthy: {
    summary: { total: 1, running: 1, retry_backoff: 0, stopped: 0, failed: 0, normal: 1, delayed: 0, abnormal: 0 },
    tasks: [
      { task: buildTask('100'), replication: buildReplication() },
    ],
    taskDetail: buildTask('100'),
    replication: buildReplication(),
    checkpoint: { file: 'mysql-bin.000001', pos: 12345 },
    lease: { owner_worker_id: 'worker-a', epoch: 7, updated_at: now },
    runs: [
      { run_id: 'run-1', worker_id: 'worker-a', epoch: 7, started_at: now, ended_at: '', end_reason: '' },
    ],
    events: [
      { sequence: 1, type: 'RUNNING', time: now, message: '任务运行正常' },
    ],
    files: [
      { file_name: 'mysql-bin.000001', size_bytes: 2048, start_pos: 4, end_pos: 12345, upload_state: 'UPLOADED', object_key: 'bucket/path/mysql-bin.000001' },
    ],
    sources: [
      { host: '127.0.0.1', port: 3306, task_count: 1, running: 1, normal: 1, delayed: 0, abnormal: 0 },
    ],
    workers: [
      { worker_id: 'worker-a', task_count: 1, running: 1, leased: 1, online: true, last_seen_at: now },
    ],
    clusterOverview: { task_count: 1, worker_count: 1, running_task_count: 1, leased_task_count: 1 },
  },
  anomaly: {
    summary: { total: 3, running: 1, retry_backoff: 0, stopped: 0, failed: 1, normal: 1, delayed: 1, abnormal: 1 },
    tasks: [
      { task: buildTask('201', { name: 'task-abnormal' }), replication: buildReplication({ status: 'ABNORMAL', reason: 'RUNNER_ERROR', error: 'replication broken', delay_seconds: 15 }) },
      { task: buildTask('202', { name: 'task-delayed' }), replication: buildReplication({ status: 'DELAYED', delay_seconds: 91, reason: 'DELAY_EXCEEDS_THRESHOLD' }) },
      { task: buildTask('203', { name: 'task-failed', state: 'FAILED' }), replication: buildReplication({ status: 'IDLE', has_progress: false }) },
    ],
    sources: [
      { host: '127.0.0.1', port: 3306, task_count: 3, running: 1, normal: 1, delayed: 1, abnormal: 1 },
    ],
    workers: [
      { worker_id: 'worker-a', task_count: 3, running: 1, leased: 1, online: true, last_seen_at: now },
    ],
    clusterOverview: { task_count: 3, worker_count: 1, running_task_count: 1, leased_task_count: 1 },
  },
  'upload-failed': {
    summary: { total: 1, running: 1, retry_backoff: 0, stopped: 0, failed: 0, normal: 0, delayed: 0, abnormal: 1 },
    tasks: [
      { task: buildTask('301', { name: 'task-upload-failed' }), replication: buildReplication({ status: 'ABNORMAL', reason: 'RUNNER_ERROR', error: 'upload failed' }) },
    ],
    taskDetail: buildTask('301', { name: 'task-upload-failed' }),
    replication: buildReplication({ status: 'ABNORMAL', reason: 'RUNNER_ERROR', error: 'upload failed' }),
    checkpoint: { file: 'mysql-bin.000020', pos: 76543 },
    lease: { owner_worker_id: 'worker-b', epoch: 11, updated_at: now },
    runs: [
      { run_id: 'run-upload-1', worker_id: 'worker-b', epoch: 11, started_at: now, ended_at: '', end_reason: '' },
    ],
    events: [
      { sequence: 1, type: 'UPLOAD_FAILED', time: now, message: '文件上传失败，等待重试' },
    ],
    filesBeforeRetry: [
      { file_name: 'mysql-bin.000020', size_bytes: 4096, start_pos: 4, end_pos: 76543, upload_state: 'UPLOAD_FAILED', object_key: 'bucket/path/mysql-bin.000020' },
      { file_name: 'mysql-bin.000021', size_bytes: 1024, start_pos: 76544, end_pos: 80000, upload_state: 'UPLOADED', object_key: 'bucket/path/mysql-bin.000021' },
    ],
    filesAfterRetry: [
      { file_name: 'mysql-bin.000020', size_bytes: 4096, start_pos: 4, end_pos: 76543, upload_state: 'UPLOADED', object_key: 'bucket/path/mysql-bin.000020' },
      { file_name: 'mysql-bin.000021', size_bytes: 1024, start_pos: 76544, end_pos: 80000, upload_state: 'UPLOADED', object_key: 'bucket/path/mysql-bin.000021' },
    ],
    sources: [
      { host: '127.0.0.1', port: 3307, task_count: 1, running: 1, normal: 0, delayed: 0, abnormal: 1 },
    ],
    workers: [
      { worker_id: 'worker-b', task_count: 1, running: 1, leased: 1, online: true, last_seen_at: now },
    ],
    clusterOverview: { task_count: 1, worker_count: 1, running_task_count: 1, leased_task_count: 1 },
  },
} as const
