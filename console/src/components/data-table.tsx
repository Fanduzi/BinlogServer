// The one table every page renders lists with. Columns are data, rows are
// activated by click or Enter (Enter is consumed so it cannot also reach
// whatever takes focus next, e.g. a drawer's close button), and an empty list
// shows the page's empty text.
import type { ReactNode } from 'react'
import { EmptyState } from '@/components/empty-state'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { cn } from '@/lib/utils'

export interface Column<T> {
  key: string
  header: ReactNode
  cell: (row: T) => ReactNode
  className?: string
  headClassName?: string
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  empty,
  emptyTestid,
  onRowActivate,
  rowClassName,
  rowProps,
  testid,
  className,
}: {
  columns: Column<T>[]
  rows: T[]
  rowKey: (row: T) => string
  empty?: ReactNode
  emptyTestid?: string
  onRowActivate?: (row: T) => void
  rowClassName?: (row: T) => string | undefined
  rowProps?: (row: T) => Record<`data-${string}`, string>
  testid?: string
  className?: string
}) {
  return (
    <div className={cn('min-w-0 overflow-x-auto rounded-md border', className)}>
      <Table data-testid={testid}>
        <TableHeader>
          <TableRow>
            {columns.map((c) => (
              <TableHead key={c.key} className={c.headClassName ?? c.className}>
                {c.header}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <TableRow
              key={rowKey(row)}
              {...rowProps?.(row)}
              className={cn(onRowActivate && 'cursor-pointer', rowClassName?.(row))}
              tabIndex={onRowActivate ? 0 : undefined}
              onClick={onRowActivate ? () => onRowActivate(row) : undefined}
              onKeyDown={
                onRowActivate
                  ? (e) => {
                      if (e.key !== 'Enter' || e.target !== e.currentTarget) return
                      e.preventDefault()
                      onRowActivate(row)
                    }
                  : undefined
              }
            >
              {columns.map((c) => (
                <TableCell key={c.key} className={c.className}>
                  {c.cell(row)}
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {rows.length === 0 && empty != null && <EmptyState testid={emptyTestid}>{empty}</EmptyState>}
    </div>
  )
}
