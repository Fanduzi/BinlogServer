// Reuses the Vue console's message catalogs so wording stays identical
// during the migration (issue #302). Interpolation follows vue-i18n's
// named "{name}" form; a missing key returns the key, as vue-i18n does.
import { createContext, useContext, useMemo, type ReactNode } from 'react'
import en from '../../../frontend/src/locales/en.json'
import zhCN from '../../../frontend/src/locales/zh-CN.json'

export type Locale = 'zh-CN' | 'en'
const SAVED_LOCALE_KEY = 'locale'
// Strings the Vue console got from Element Plus or that only the new Console
// needs. Everything else comes from the shared catalogs above.
const consoleOnly = {
  'zh-CN': {
    pagination: { total: '共 {total} 条', perPage: '{size}条/页', prev: '上一页', next: '下一页' },
    migration: {
      pendingTitle: '此页面尚未迁移',
      pendingBody: '新版控制台正按页迁移（#302）。请先在现有控制台中使用此页面。',
      openClassic: '打开现有控制台',
      formPending: '编辑和接管表单将在后续迁移 PR 中提供，请暂用现有控制台。',
      notFoundTitle: '页面不存在',
      notFoundBody: '这个地址没有对应的页面。',
      backToTasks: '返回任务列表',
    },
  },
  en: {
    pagination: { total: 'Total {total}', perPage: '{size}/page', prev: 'Previous', next: 'Next' },
    migration: {
      pendingTitle: 'This page has not moved yet',
      pendingBody: 'The new Console is moving over page by page (#302). Use this page in the current console for now.',
      openClassic: 'Open current console',
      formPending: 'The edit and adopt forms move over in a later migration PR; use the current console for now.',
      notFoundTitle: 'Page not found',
      notFoundBody: 'Nothing lives at this address.',
      backToTasks: 'Back to tasks',
    },
  },
}

const catalogs: Record<Locale, unknown> = {
  'zh-CN': { ...zhCN, ...consoleOnly['zh-CN'] },
  en: { ...en, ...consoleOnly.en },
}

export function getLocale(): Locale {
  const saved = localStorage.getItem(SAVED_LOCALE_KEY)
  if (saved === 'zh-CN' || saved === 'en') return saved
  return 'zh-CN'
}

export function setLocale(locale: Locale) {
  localStorage.setItem(SAVED_LOCALE_KEY, locale)
  document.documentElement.lang = locale
}

function lookup(catalog: unknown, key: string): string | undefined {
  let node: unknown = catalog
  for (const part of key.split('.')) {
    if (node && typeof node === 'object' && part in node) {
      node = (node as Record<string, unknown>)[part]
    } else {
      return undefined
    }
  }
  return typeof node === 'string' ? node : undefined
}

export type TFunction = (key: string, params?: Record<string, unknown>) => string

export function makeT(locale: Locale): TFunction {
  return (key, params) => {
    const msg = lookup(catalogs[locale], key) ?? lookup(catalogs.en, key) ?? key
    if (!params) return msg
    return msg.replace(/\{(\w+)\}/g, (m, name: string) =>
      name in params ? String(params[name]) : m,
    )
  }
}

const I18nContext = createContext<{ locale: Locale; t: TFunction }>({
  locale: 'zh-CN',
  t: makeT('zh-CN'),
})

export function I18nProvider({ children }: { children: ReactNode }) {
  const locale = getLocale()
  // Stable across renders so hooks can depend on t without refetch loops.
  const value = useMemo(() => ({ locale, t: makeT(locale) }), [locale])
  return (
    <I18nContext.Provider value={value}>
      {children}
    </I18nContext.Provider>
  )
}

export function useI18n() {
  return useContext(I18nContext)
}
