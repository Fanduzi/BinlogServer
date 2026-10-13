// Keeps GET /api/dashboard in step with the task pager and the server-side
// state filter, mounted once in the shell so switching Tasks <-> Alerts does
// not refetch. Mirrors App.vue's watchers:
//  - state or page size change while not on page 1 -> back to page 1 first;
//  - otherwise any state/page/size change -> refresh;
//  - any current-page filter change -> page 1;
//  - server total shrinks below the page -> clamp to the last page.
import { useEffect, useRef } from 'react'
import { useObservation } from '@/context/observation'
import { paginationParams } from './filter'
import { useTaskFilter } from './filter-context'
import { useDebounced } from './use-debounced'

export function TaskObservationSync() {
  const { refresh, dashboard } = useObservation()
  const { filter, pager, setPager } = useTaskFilter()
  const prev = useRef<[string, number, number] | null>(null)

  useEffect(() => {
    const p = prev.current
    prev.current = [filter.taskState, pager.page, pager.pageSize]
    if (p && (filter.taskState !== p[0] || pager.pageSize !== p[2]) && pager.page !== 1) {
      setPager((x) => ({ ...x, page: 1 }))
      return
    }
    void refresh(paginationParams(pager.page, pager.pageSize, filter.taskState))
  }, [filter.taskState, pager.page, pager.pageSize, refresh, setPager])

  const kw = useDebounced(filter.keyword, 300)
  const skw = useDebounced(filter.sourceKeyword, 300)
  const first = useRef(true)
  useEffect(() => {
    if (first.current) {
      first.current = false
      return
    }
    setPager((x) => (x.page === 1 ? x : { ...x, page: 1 }))
  }, [kw, skw, filter.taskState, filter.replicationStatus, filter.sortBy, filter.onlyAlert, setPager])

  useEffect(() => {
    const maxPage = Math.max(1, Math.ceil(dashboard.total / pager.pageSize))
    if (pager.page > maxPage) setPager((x) => ({ ...x, page: maxPage }))
  }, [dashboard.total, pager.pageSize, pager.page, setPager])

  return null
}
