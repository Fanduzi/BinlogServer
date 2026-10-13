import { RotateCw, Settings } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import { SidebarTrigger } from '@/components/ui/sidebar'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'

export function Header({ onSettings }: { onSettings: () => void }) {
  const { t } = useI18n()
  const { loading, refresh } = useObservation()
  return (
    <header className='bg-background sticky top-0 z-10 flex h-16 shrink-0 items-center gap-3 border-b px-4'>
      <SidebarTrigger className='-ml-1' />
      <Separator orientation='vertical' className='h-6' />
      <div className='min-w-0 flex-1'>
        <h1 className='truncate text-base font-semibold'>{t('app.title')}</h1>
        <p className='text-muted-foreground hidden truncate text-xs md:block'>
          {t('app.description')}
        </p>
      </div>
      <Button variant='outline' size='sm' disabled={loading} onClick={() => void refresh()}>
        <RotateCw className={loading ? 'animate-spin' : ''} />
        {t('btn.refresh')}
      </Button>
      <Button variant='outline' size='sm' onClick={onSettings}>
        <Settings />
        {t('btn.settings')}
      </Button>
    </header>
  )
}
