import { createFileRoute, redirect } from '@tanstack/react-router'

// Overview has not moved yet (#302); land on Tasks until it does.
export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/tasks' })
  },
})
