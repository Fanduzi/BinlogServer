import { createFileRoute, redirect } from '@tanstack/react-router'

// The Vue console opens on Overview (App.vue parseHashRoute default).
export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/overview' })
  },
})
