import { createFileRoute } from '@tanstack/react-router'
import { PendingPage } from '@/components/pending-page'

export const Route = createFileRoute('/workers')({
  component: () => <PendingPage view='workers' />,
})
