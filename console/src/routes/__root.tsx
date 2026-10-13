import { createRootRoute } from '@tanstack/react-router'
import { AuthenticatedLayout } from '@/components/layout/authenticated-layout'
import { NotFound } from '@/components/not-found'

export const Route = createRootRoute({
  component: AuthenticatedLayout,
  notFoundComponent: NotFound,
})
