// Sources view: lookup card, then source coverage for the current query.
import { SourceCoveragePanel } from '../cluster/source-coverage'
import { SourceLookupPanel } from './lookup-panel'

export function SourcesPage() {
  return (
    <div className='flex min-w-0 flex-col gap-4'>
      <SourceLookupPanel />
      <SourceCoveragePanel />
    </div>
  )
}
