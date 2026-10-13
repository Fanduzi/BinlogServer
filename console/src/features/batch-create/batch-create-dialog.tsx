// Batch create dialog (BatchCreateDialog.vue + useBatchCreate.js): preview and
// validate locally, then one POST /api/tasks/batch; Auto Start then starts each
// created task with POST /api/tasks/{id}/start, as the Vue console does.
import { createContext, useContext, useMemo, useState, type ReactNode } from 'react'
import { Loader2 } from 'lucide-react'
import { toast } from 'sonner'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useObservation } from '@/context/observation'
import { createTasksBatch, startTask } from '@/lib/api'
import { useI18n } from '@/lib/i18n'
import { parseErr } from '../task-detail/detail-format'
import { ToneBadge } from '../tasks/tone-badge'
import {
  batchItemError,
  buildBatchPayloads,
  buildPreview,
  defaultBatchForm,
  emptyPreview,
  type BatchForm,
} from './batch'

const BatchContext = createContext<{ openBatchCreate: () => void } | null>(null)

export function useBatchCreate() {
  const ctx = useContext(BatchContext)
  if (!ctx) throw new Error('useBatchCreate outside BatchCreateProvider')
  return ctx
}

const num = (v: string) => (v.trim() === '' ? 0 : Number(v))
const atLeast = (v: number, min: number) => (Number.isFinite(v) ? Math.max(min, v) : min)

export function BatchCreateProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n()
  const { refresh } = useObservation()
  const [open, setOpen] = useState(false)
  const [form, setForm] = useState<BatchForm>(defaultBatchForm)
  const [preview, setPreview] = useState(emptyPreview)
  const [busy, setBusy] = useState(false)
  const [failures, setFailures] = useState<string[]>([])

  const api = useMemo(
    () => ({
      openBatchCreate: () => {
        setForm(defaultBatchForm())
        setPreview(emptyPreview())
        setOpen(true)
      },
    }),
    [],
  )
  const set = (patch: Partial<BatchForm>) => setForm((f) => ({ ...f, ...patch }))

  function onPreview() {
    const { preview: next, tooManyError } = buildPreview(t, form.lines)
    setPreview(next)
    if (tooManyError) {
      toast.error(tooManyError)
    } else if (!next.rows.length || next.errors.length > 0) {
      toast.warning(
        t('msg.previewComplete', {
          valid: next.validCount,
          errors: next.errors.length,
        }),
      )
    } else {
      toast.success(t('msg.previewPassed', { count: next.validCount }))
    }
  }

  async function onSubmit() {
    if (busy || !preview.canSubmit) return
    setBusy(true)
    try {
      const { errors, submittedRows, payloads } = buildBatchPayloads(t, form, preview.rows)
      let success = 0
      if (payloads.length) {
        const results = await createTasksBatch({ items: payloads })
        for (const result of results) {
          const row = submittedRows[Number(result?.index)]
          const label = row
            ? `${row.name}(${row.host}:${row.port})`
            : result?.cluster_key || 'batch item'
          if (result?.task) {
            try {
              if (form.autoStart) await startTask(result.task.id)
              success += 1
            } catch (err) {
              errors.push(`${label} -> ${parseErr(err)}`)
            }
            continue
          }
          errors.push(`${label} -> ${batchItemError(result?.error)}`)
        }
      }
      await refresh()
      if (!errors.length) {
        toast.success(t('msg.batchCreateSuccess', { count: success }))
        setOpen(false)
        return
      }
      toast.warning(t('msg.batchPartialSuccess', { success, failed: errors.length }))
      setFailures(errors)
    } catch (err) {
      toast.error(parseErr(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <BatchContext.Provider value={api}>
      {children}
      <Dialog open={open} onOpenChange={(v) => !busy && setOpen(v)}>
        <DialogContent
          data-testid='batch-create'
          className='max-h-[90vh] overflow-y-auto sm:max-w-[860px]'
        >
          <DialogHeader>
            <DialogTitle>{t('batch.title')}</DialogTitle>
            <DialogDescription>
              {t('batch.formatHint')} <code>name,host,port</code> {t('batch.formatOr')}{' '}
              <code>host,port</code> {t('batch.formatOr')} <code>host:port</code>
            </DialogDescription>
          </DialogHeader>
          <div className='grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2'>
            <div className='flex min-w-0 flex-col gap-3'>
              <Field label={t('form.replUser')}>
                <Input
                  data-testid='batch-user'
                  value={form.user}
                  onChange={(e) => set({ user: e.target.value })}
                />
              </Field>
              <Field label={t('form.replPassword')}>
                <Input
                  data-testid='batch-password'
                  type='password'
                  autoComplete='new-password'
                  value={form.password}
                  onChange={(e) => set({ password: e.target.value })}
                />
              </Field>
              <Field label={t('form.flavor')} hint={t('form.flavorHint')}>
                <Select value={form.flavor} onValueChange={(v) => set({ flavor: v })}>
                  <SelectTrigger data-testid='batch-flavor' className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value='mysql'>{t('form.flavorMysql')}</SelectItem>
                    <SelectItem value='mariadb'>{t('form.flavorMariaDB')}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
              <Field label={t('form.serverIdStart')}>
                <Input
                  data-testid='batch-server-id-start'
                  type='number'
                  min={1}
                  value={form.serverIdStart}
                  onChange={(e) => set({ serverIdStart: num(e.target.value) })}
                  onBlur={() => set({ serverIdStart: atLeast(form.serverIdStart, 1) })}
                />
              </Field>
              <Field label={t('form.retentionDays')}>
                <Input
                  data-testid='batch-retention'
                  type='number'
                  min={1}
                  value={form.retentionDays}
                  onChange={(e) => set({ retentionDays: num(e.target.value) })}
                  onBlur={() => set({ retentionDays: atLeast(form.retentionDays, 1) })}
                />
              </Field>
              <div className='flex flex-wrap gap-6'>
                <Field label={t('form.semiSync')}>
                  <Switch checked={form.semiSync} onCheckedChange={(v) => set({ semiSync: v })} />
                </Field>
                <Field label={t('form.autoStart')}>
                  <Switch
                    data-testid='batch-auto-start'
                    checked={form.autoStart}
                    onCheckedChange={(v) => set({ autoStart: v })}
                  />
                </Field>
              </div>
            </div>
            <textarea
              data-testid='batch-lines'
              rows={14}
              placeholder={t('placeholder.hostExample')}
              value={form.lines}
              onChange={(e) => {
                set({ lines: e.target.value })
                // Editing the lines invalidates an earlier preview (App.vue watch).
                if (preview.ready) setPreview(emptyPreview())
              }}
              className='border-input focus-visible:border-ring focus-visible:ring-ring/50 dark:bg-input/30 min-h-48 w-full min-w-0 rounded-md border bg-transparent px-3 py-2 font-mono text-sm shadow-xs outline-none focus-visible:ring-[3px]'
            />
          </div>
          <div className='flex min-w-0 flex-col gap-2'>
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <div
                className='flex flex-wrap items-center gap-2 text-sm'
                data-testid='batch-preview-status'
              >
                <span>{t('batch.previewStatus')}</span>
                {!preview.ready && <ToneBadge tone='info'>{t('batch.notPreviewed')}</ToneBadge>}
                {preview.ready && (
                  <ToneBadge tone='info'>
                    {t('batch.validErrorCount', {
                      valid: preview.validCount,
                      errors: preview.errors.length,
                    })}
                  </ToneBadge>
                )}
                {preview.ready && preview.canSubmit && (
                  <ToneBadge tone='success'>{t('batch.submittable')}</ToneBadge>
                )}
                {preview.ready && !preview.canSubmit && (
                  <ToneBadge tone='danger'>{t('batch.hasErrors')}</ToneBadge>
                )}
              </div>
              <Button variant='outline' size='sm' data-testid='batch-preview' onClick={onPreview}>
                {t('btn.previewValidate')}
              </Button>
            </div>
            {preview.rows.length > 0 && (
              <div className='max-h-72 min-w-0 overflow-auto rounded-md border'>
                <Table data-testid='batch-preview-table'>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{t('table.lineNo')}</TableHead>
                      <TableHead>{t('form.taskName')}</TableHead>
                      <TableHead>{t('batch.host')}</TableHead>
                      <TableHead>{t('batch.port')}</TableHead>
                      <TableHead>{t('table.validation')}</TableHead>
                      <TableHead>{t('batch.reason')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {preview.rows.map((row) => (
                      <TableRow key={row.lineNo} data-valid={row.valid ? 'true' : 'false'}>
                        <TableCell>{row.lineNo}</TableCell>
                        <TableCell className='break-all whitespace-normal'>{row.name}</TableCell>
                        <TableCell className='break-all whitespace-normal'>{row.host}</TableCell>
                        <TableCell>{row.port}</TableCell>
                        <TableCell>
                          <ToneBadge tone={row.valid ? 'success' : 'danger'}>
                            {row.valid ? t('batch.passed') : t('batch.failed')}
                          </ToneBadge>
                        </TableCell>
                        <TableCell className='min-w-48 whitespace-normal'>{row.error}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            )}
          </div>
          <DialogFooter>
            <Button variant='outline' disabled={busy} onClick={() => setOpen(false)}>
              {t('btn.cancel')}
            </Button>
            <Button
              data-testid='batch-submit'
              disabled={!preview.canSubmit || busy}
              onClick={() => void onSubmit()}
            >
              {busy && <Loader2 className='animate-spin' />}
              {t('btn.startBatchCreate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <AlertDialog open={failures.length > 0} onOpenChange={(v) => !v && setFailures([])}>
        <AlertDialogContent data-testid='batch-failures'>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('msg.batchCreateFailedDetail')}</AlertDialogTitle>
            <AlertDialogDescription asChild>
              <pre className='bg-muted max-h-72 overflow-auto rounded-md p-3 text-xs whitespace-pre-wrap'>
                {failures.join('\n')}
              </pre>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogAction>{t('btn.gotIt')}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </BatchContext.Provider>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <div className='flex min-w-0 flex-col gap-1.5'>
      <Label>{label}</Label>
      {children}
      {hint && <p className='text-muted-foreground text-xs leading-snug'>{hint}</p>}
    </div>
  )
}
