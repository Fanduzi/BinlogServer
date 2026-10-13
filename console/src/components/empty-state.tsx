import { Inbox } from 'lucide-react'
import type { ReactNode } from 'react'

export function EmptyState({ children, testid }: { children: ReactNode; testid?: string }) {
  return (
    <div
      className='text-muted-foreground flex flex-col items-center gap-2 py-10 text-center text-sm'
      data-testid={testid}
    >
      <Inbox className='size-8 opacity-50' />
      {children}
    </div>
  )
}
