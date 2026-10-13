// The card every Console page is built from (the Vue panel-card): an icon and
// title, an optional hint or actions on the right, then the body.
import type { ComponentType, ReactNode } from 'react'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { cn } from '@/lib/utils'

export function Panel({
  icon: Icon,
  title,
  hint,
  actions,
  children,
  className,
  testid,
}: {
  icon?: ComponentType<{ className?: string }>
  title: ReactNode
  hint?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
  testid?: string
}) {
  return (
    <Card className={cn('min-w-0 gap-3 py-4', className)} data-testid={testid}>
      <CardHeader className='flex flex-row flex-wrap items-center justify-between gap-2 px-4'>
        <CardTitle className='flex items-center gap-2 text-sm'>
          {Icon && <Icon className='size-4' />}
          {title}
        </CardTitle>
        {(hint != null || actions) && (
          <div className='flex flex-wrap items-center gap-2'>
            {hint != null && <span className='text-muted-foreground text-xs'>{hint}</span>}
            {actions}
          </div>
        )}
      </CardHeader>
      {children != null && <CardContent className='min-w-0 px-4'>{children}</CardContent>}
    </Card>
  )
}

export function StatCell({
  label,
  value,
  testid,
}: {
  label: ReactNode
  value: ReactNode
  testid?: string
}) {
  return (
    <div className='bg-muted/40 min-w-0 rounded-lg border px-3 py-2'>
      <p className='text-muted-foreground text-xs'>{label}</p>
      <strong className='block text-2xl tabular-nums' data-testid={testid}>
        {value}
      </strong>
    </div>
  )
}
