// Placeholder for a view that has not moved to the new Console yet.
import { ExternalLink } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useI18n } from '@/lib/i18n'

export function PendingPage({ view }: { view: string }) {
  const { t } = useI18n()
  return (
    <Card data-testid={`pending-${view}`}>
      <CardHeader>
        <CardTitle>
          {t(`nav.${view}`)} · {t('migration.pendingTitle')}
        </CardTitle>
        <CardDescription>{t('migration.pendingBody')}</CardDescription>
      </CardHeader>
      <CardContent>
        <Button asChild variant='outline'>
          <a href={`/ui/#/${view}`}>
            <ExternalLink />
            {t('migration.openClassic')}
          </a>
        </Button>
      </CardContent>
    </Card>
  )
}
