// Loads the drawer the way useTaskDetail.showDetail does: seven requests
// together, then /lease and /runs allowed to fail.
import { useCallback, useEffect, useRef, useState } from 'react'
import {
  getCheckpoint,
  getRecoveryWindow,
  getReplication,
  getTask,
  getTaskLease,
  listEvents,
  listFiles,
  listReplay,
  listTaskRuns,
  type AnyRecord,
  type Task,
} from '@/lib/api'
import { errorText } from '@/lib/errors'
import { createLatest } from '@/lib/latest'
import { useI18n } from '@/lib/i18n'
import { notifyError } from '@/lib/notify'

export const RUN_HISTORY_LIMIT = 10
// Files window the drawer loads. Replay and its archive take no limit (#305).
export const TASK_DETAIL_INVENTORY_LIMIT = 80

export interface TaskDetail {
  task: Task & AnyRecord
  replication: AnyRecord | null
  lease: AnyRecord | null
  runs: AnyRecord[]
  checkpoint: AnyRecord | null
  events: AnyRecord[]
  files: AnyRecord[]
  replay: AnyRecord
  recovery: AnyRecord
}

export function useTaskDetail(taskId: string) {
  const { t } = useI18n()
  const [detail, setDetail] = useState<TaskDetail | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  // A reload for an older task (or an older reload) must not overwrite a newer one.
  const latest = useRef(createLatest()).current

  const load = useCallback(async () => {
    const id = taskId
    const token = latest.begin()
    try {
      setLoading(true)
      const [task, cp, evs, fs, replaySet, replication, recoveryWindow] = await Promise.all([
        getTask(id),
        getCheckpoint(id),
        listEvents(id, 120),
        listFiles(id, TASK_DETAIL_INVENTORY_LIMIT),
        listReplay(id),
        getReplication(id),
        getRecoveryWindow(id),
      ])
      const [leaseResult, runsResult] = await Promise.allSettled([
        getTaskLease(id),
        listTaskRuns(id, RUN_HISTORY_LIMIT),
      ])
      const lease = leaseResult.status === 'fulfilled' ? leaseResult.value : null
      const runs = runsResult.status === 'fulfilled' ? runsResult.value : []
      if (!latest.isLatest(token)) return
      setDetail({
        task,
        replication,
        lease,
        runs: Array.isArray(runs) ? runs : [],
        checkpoint: cp,
        events: evs || [],
        files: fs || [],
        replay: replaySet || { paths: [] },
        recovery: recoveryWindow || { continuous: true, breaks: [] },
      })
      setError('')
    } catch (err) {
      if (!latest.isLatest(token)) return
      const fallback = t('msg.unknownError')
      setError(errorText(err, fallback))
      notifyError(err, fallback)
    } finally {
      if (latest.isLatest(token)) setLoading(false)
    }
  }, [taskId, t, latest])

  useEffect(() => {
    setDetail(null)
    setError('')
    void load()
    return () => latest.cancel()
  }, [load, latest])

  const setFiles = useCallback((files: AnyRecord[]) => {
    setDetail((d) => (d ? { ...d, files } : d))
  }, [])

  return { detail, error, loading, reload: load, setFiles }
}
