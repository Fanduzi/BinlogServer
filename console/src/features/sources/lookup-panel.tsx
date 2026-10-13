// "Source Lookup" card (App.vue activeView === 'sources').
import { Search } from 'lucide-react'
import { NumberInput } from '@/components/number-input'
import { Panel } from '@/components/panel'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useObservation } from '@/context/observation'
import { useI18n } from '@/lib/i18n'
import { formatTs } from '../tasks/format'
import { ToneBadge } from '../tasks/tone-badge'
import { useSourceLookup } from './lookup-context'

export function SourceLookupPanel() {
  const { t, locale } = useI18n()
  const { loading, dashboard } = useObservation()
  const { query, setQuery, lookup, apply, clear } = useSourceLookup()
  return (
    <Panel
      icon={Search}
      title={t('source.lookupTitle')}
      hint={t('source.lookupHint')}
      testid='source-lookup'
    >
      <form
        className='flex flex-wrap items-center gap-2'
        onSubmit={(e) => {
          e.preventDefault()
          void apply()
        }}
      >
        <Input
          className='w-64'
          value={query.host}
          placeholder={t('placeholder.hostExample')}
          onChange={(e) => setQuery({ ...query, host: e.target.value })}
          aria-label='host'
          data-testid='source-host-input'
        />
        <NumberInput
          className='w-32'
          nullable
          min={1}
          max={65535}
          value={query.port}
          onValueChange={(port) => setQuery({ ...query, port })}
          aria-label='port'
          data-testid='source-port-input'
        />
        <Button type='submit' disabled={loading} data-testid='source-query'>
          {t('btn.query')}
        </Button>
        <Button
          type='button'
          variant='outline'
          onClick={() => void clear()}
          data-testid='source-clear'
        >
          {t('btn.clear')}
        </Button>
      </form>
      <div className='text-muted-foreground mt-3 flex flex-wrap gap-4 text-xs'>
        <span data-testid='source-threshold'>
          {t('source.delayThreshold', { seconds: dashboard.threshold_seconds })}
        </span>
        <span>{formatTs(locale, dashboard.generated_at)}</span>
      </div>
      {lookup.checked && (
        <div className='mt-3 flex items-center gap-2 text-sm' data-testid='source-lookup-result'>
          <ToneBadge tone={lookup.exists ? 'success' : 'info'} data-exists={String(lookup.exists)}>
            {lookup.exists ? t('source.taskExists') : t('source.taskNotFound')}
          </ToneBadge>
          <span data-testid='source-match-count'>
            {t('source.matchCount', { count: lookup.count })}
          </span>
        </div>
      )}
    </Panel>
  )
}
