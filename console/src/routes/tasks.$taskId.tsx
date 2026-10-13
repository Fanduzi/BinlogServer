import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { TaskDetailSheet } from '@/features/task-detail/task-detail-sheet'

function TaskDetailRoute() {
  const { taskId } = Route.useParams()
  const navigate = useNavigate()
  return <TaskDetailSheet taskId={taskId} onClose={() => void navigate({ to: '/tasks' })} />
}

export const Route = createFileRoute('/tasks/$taskId')({ component: TaskDetailRoute })
