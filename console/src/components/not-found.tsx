import { Link } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useI18n } from '@/lib/i18n'

export function NotFound() {
  const { t } = useI18n()
  return (
    <Card data-testid='not-found'>
      <CardHeader>
        <CardTitle>{t('migration.notFoundTitle')}</CardTitle>
        <CardDescription>{t('migration.notFoundBody')}</CardDescription>
      </CardHeader>
      <CardContent>
        <Button asChild variant='outline'>
          <Link to='/tasks'>{t('migration.backToTasks')}</Link>
        </Button>
      </CardContent>
    </Card>
  )
}
