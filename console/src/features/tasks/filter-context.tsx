// Task filter and pager state shared by the Tasks and Alerts routes (one
// state, two views, as App.vue's uiFilter/pager), so it survives switching.
import { createContext, useContext, useState, type ReactNode } from 'react'
import { defaultUiFilter, type QuickFilter, type UiFilter } from './filter'

interface Pager {
  page: number
  pageSize: number
}

interface TaskFilterValue {
  filter: UiFilter
  setFilter: (f: UiFilter | ((prev: UiFilter) => UiFilter)) => void
  pager: Pager
  setPager: (p: Pager | ((prev: Pager) => Pager)) => void
  activeQuick: QuickFilter
  setActiveQuick: (q: QuickFilter) => void
}

const TaskFilterContext = createContext<TaskFilterValue | null>(null)

export function TaskFilterProvider({ children }: { children: ReactNode }) {
  const [filter, setFilter] = useState<UiFilter>(defaultUiFilter)
  const [pager, setPager] = useState<Pager>({ page: 1, pageSize: 20 })
  const [activeQuick, setActiveQuick] = useState<QuickFilter>('all')
  return (
    <TaskFilterContext.Provider
      value={{ filter, setFilter, pager, setPager, activeQuick, setActiveQuick }}
    >
      {children}
    </TaskFilterContext.Provider>
  )
}

export function useTaskFilter() {
  const ctx = useContext(TaskFilterContext)
  if (!ctx) throw new Error('useTaskFilter must be used inside TaskFilterProvider')
  return ctx
}
