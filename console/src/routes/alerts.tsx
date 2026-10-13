import { useEffect } from 'react'
import { Outlet, createFileRoute } from '@tanstack/react-router'
import { quickFilterState } from '@/features/tasks/filter'
import { useTaskFilter } from '@/features/tasks/filter-context'
import { TasksPage } from '@/features/tasks/tasks-page'

// Alerts is the task list with alert-only on and both status filters cleared
// (App.vue activateAlertsView).
function AlertsRoute() {
  const { setFilter, setActiveQuick } = useTaskFilter()
  useEffect(() => {
    setActiveQuick('alert')
    setFilter((f) => quickFilterState('alert', f))
  }, [setFilter, setActiveQuick])
  return (
    <TasksPage view='alerts'>
      <Outlet />
    </TasksPage>
  )
}

export const Route = createFileRoute('/alerts')({ component: AlertsRoute })
