import { createFileRoute, redirect } from '@tanstack/react-router'
import { TasksPage } from '@/features/tasks/tasks-page'

export const Route = createFileRoute('/tasks')({
  // The Vue console treated #/tasks?mode=alerts as the Alerts view.
  validateSearch: (search: Record<string, unknown>) =>
    search.mode === 'alerts' ? { mode: 'alerts' as const } : {},
  beforeLoad: ({ search }) => {
    if ((search as { mode?: string }).mode === 'alerts') throw redirect({ to: '/alerts' })
  },
  component: () => <TasksPage view='tasks' />,
})
