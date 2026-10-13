// Settings: UI language and API token, as frontend/src/components/SettingsDialog.vue.
import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
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
import { getAuthToken } from '@/lib/auth'
import { useI18n, type Locale } from '@/lib/i18n'

export function SettingsDialog({
  open,
  onOpenChange,
  onLocaleChange,
  onSave,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onLocaleChange: (locale: Locale) => void
  onSave: (token: string) => void
}) {
  const { t, locale } = useI18n()
  const [token, setToken] = useState('')

  useEffect(() => {
    if (open) setToken(getAuthToken() || '')
  }, [open])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid='settings-dialog' className='sm:max-w-[480px]'>
        <DialogHeader>
          <DialogTitle>{t('settings.title')}</DialogTitle>
        </DialogHeader>
        <div className='grid gap-4'>
          <div className='grid gap-2'>
            <Label>{t('settings.language')}</Label>
            <Select value={locale} onValueChange={(v) => onLocaleChange(v as Locale)}>
              <SelectTrigger className='w-full'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='zh-CN'>{t('settings.langZhCN')}</SelectItem>
                <SelectItem value='en'>{t('settings.langEn')}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className='grid gap-2'>
            <Label htmlFor='settings-token'>{t('settings.apiToken')}</Label>
            <Input
              id='settings-token'
              data-testid='settings-token-input'
              type='password'
              value={token}
              placeholder={t('auth.tokenPlaceholder')}
              onChange={(e) => setToken(e.target.value)}
            />
            <p className='text-muted-foreground text-xs'>{t('auth.tokenHint')}</p>
          </div>
        </div>
        <DialogFooter>
          <Button variant='outline' onClick={() => onOpenChange(false)}>
            {t('btn.cancel')}
          </Button>
          <Button onClick={() => onSave(token)}>{t('btn.save')}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
