import { createFileRoute } from '@tanstack/react-router'
import { WorkersPage } from '@/features/workers/workers-page'

export const Route = createFileRoute('/workers')({ component: WorkersPage })
