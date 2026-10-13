// Source lookup state (useSourceLookup.js). It lives above the router so the
// typed host/port and the last result survive view switches, as in Vue.
// Query applies host/port to the shared dashboard query (task pager back to
// page 1) and runs GET /api/sources/lookup next to the refresh; Clear drops
// both and refreshes.
import { createContext, useCallback, useContext, useState, type ReactNode } from 'react'
import { toast } from 'sonner'
import { useObservation } from '@/context/observation'
import { lookupSource } from '@/lib/api'
import { useI18n } from '@/lib/i18n'
import { notifyError } from '@/lib/notify'
import { paginationParams } from '../tasks/filter'
import { useTaskFilter } from '../tasks/filter-context'
import {
  buildSourceFilter,
  emptyLookup,
  emptySourceQuery,
  lookupParams,
  lookupResult,
  type LookupResult,
  type SourceQuery,
} from './source-filter'

interface SourceLookupValue {
  query: SourceQuery
  setQuery: (q: SourceQuery) => void
  lookup: LookupResult
  apply: () => Promise<void>
  clear: () => Promise<void>
}

const Ctx = createContext<SourceLookupValue | null>(null)

export function SourceLookupProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n()
  const { refresh } = useObservation()
  const { filter, pager, setPager } = useTaskFilter()
  const [query, setQuery] = useState<SourceQuery>(emptySourceQuery)
  const [lookup, setLookup] = useState<LookupResult>(emptyLookup)

  const apply = useCallback(async () => {
    const params = lookupParams(query)
    if (!params) {
      toast.error(t('msg.hostPortRequired'))
      return
    }
    // Page 1 goes into the same refresh; the pager sync sees an unchanged query and skips.
    setPager((x) => (x.page === 1 ? x : { ...x, page: 1 }))
    try {
      const [resp] = await Promise.all([
        lookupSource(params),
        refresh({
          source: buildSourceFilter(query),
          paging: paginationParams(1, pager.pageSize, filter.taskState),
        }),
      ])
      setLookup(lookupResult(resp))
    } catch (err) {
      notifyError(err)
    }
  }, [query, t, refresh, setPager, pager.pageSize, filter.taskState])

  const clear = useCallback(async () => {
    setQuery(emptySourceQuery)
    setLookup(emptyLookup)
    await refresh({ source: {} })
  }, [refresh])

  return <Ctx.Provider value={{ query, setQuery, lookup, apply, clear }}>{children}</Ctx.Provider>
}

export function useSourceLookup() {
  const v = useContext(Ctx)
  if (!v) throw new Error('useSourceLookup must be used inside SourceLookupProvider')
  return v
}
