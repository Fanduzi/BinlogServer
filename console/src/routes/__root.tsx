import { createRootRoute } from '@tanstack/react-router'
import { AuthenticatedLayout } from '@/components/layout/authenticated-layout'

export const Route = createRootRoute({ component: AuthenticatedLayout })
