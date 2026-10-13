import type { ReactNode } from 'react'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import type { Tone } from './format'

const toneClass: Record<Tone, string> = {
  success: 'border-emerald-200 bg-emerald-50 text-emerald-700',
  warning: 'border-amber-200 bg-amber-50 text-amber-700',
  danger: 'border-red-200 bg-red-50 text-red-700',
  info: 'border-slate-200 bg-slate-50 text-slate-600',
}

export function ToneBadge({
  tone,
  children,
  ...rest
}: { tone: Tone; children: ReactNode } & Record<`data-${string}`, string>) {
  return (
    <Badge variant='outline' className={cn('font-medium', toneClass[tone])} {...rest}>
      {children}
    </Badge>
  )
}
