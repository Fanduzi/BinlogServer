import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { TaskDetailSheet } from '@/features/task-detail/task-detail-sheet'

function AlertDetailRoute() {
  const { taskId } = Route.useParams()
  const navigate = useNavigate()
  return <TaskDetailSheet taskId={taskId} onClose={() => void navigate({ to: '/alerts' })} />
}

export const Route = createFileRoute('/alerts/$taskId')({ component: AlertDetailRoute })
