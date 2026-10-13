import { createFileRoute } from '@tanstack/react-router'
import { PendingPage } from '@/components/pending-page'

export const Route = createFileRoute('/overview')({
  component: () => <PendingPage view='overview' />,
})
