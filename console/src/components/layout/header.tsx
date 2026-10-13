import { Layers, Plus, RotateCw, Settings } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { SidebarTrigger } from '@/components/ui/sidebar'
import { useObservation } from '@/context/observation'
import { useBatchCreate } from '@/features/batch-create/batch-create-dialog'
import { useTaskForm } from '@/features/task-form/task-form-dialog'
import { useI18n } from '@/lib/i18n'

export function Header({ onSettings }: { onSettings: () => void }) {
  const { t } = useI18n()
  const { loading, refresh } = useObservation()
  const { openCreate } = useTaskForm()
  const { openBatchCreate } = useBatchCreate()
  return (
    <header className='bg-background sticky top-0 z-10 flex h-16 shrink-0 items-center gap-2 border-b px-4 sm:gap-3'>
      <SidebarTrigger className='-ml-1' />
      <Separator orientation='vertical' className='h-6' />
      <div className='min-w-0 flex-1'>
        <h1 className='truncate text-base font-semibold'>{t('app.title')}</h1>
        <p className='text-muted-foreground hidden truncate text-xs md:block'>
          {t('app.description')}
        </p>
      </div>
      <Button
        size='sm'
        data-testid='create-task'
        aria-label={t('btn.createTask')}
        onClick={openCreate}
      >
        <Plus />
        <span className='hidden sm:inline'>{t('btn.createTask')}</span>
      </Button>
      <Button
        variant='outline'
        size='sm'
        data-testid='batch-create-open'
        aria-label={t('btn.batchCreate')}
        onClick={openBatchCreate}
      >
        <Layers />
        <span className='hidden sm:inline'>{t('btn.batchCreate')}</span>
      </Button>
      <Button
        variant='outline'
        size='sm'
        aria-label={t('btn.refresh')}
        disabled={loading}
        onClick={() => void refresh()}
      >
        <RotateCw className={loading ? 'animate-spin' : ''} />
        <span className='hidden sm:inline'>{t('btn.refresh')}</span>
      </Button>
      <Button variant='outline' size='sm' aria-label={t('btn.settings')} onClick={onSettings}>
        <Settings />
        <span className='hidden sm:inline'>{t('btn.settings')}</span>
      </Button>
    </header>
  )
}
