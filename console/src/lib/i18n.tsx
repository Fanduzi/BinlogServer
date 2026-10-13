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
type Tree = { [k: string]: unknown }
const isTree = (v: unknown): v is Tree => !!v && typeof v === 'object' && !Array.isArray(v)

/** Deep merge, so a console-only key never replaces a whole shared namespace. */
export function mergeCatalog(base: Tree, extra: Tree): Tree {
  const out: Tree = { ...base }
  for (const [k, v] of Object.entries(extra)) {
    out[k] = isTree(v) && isTree(out[k]) ? mergeCatalog(out[k] as Tree, v) : v
  }
  return out
}

const consoleOnly = {
  'zh-CN': {
    workersView: { status: '状态', load: '任务', heartbeat: '心跳' },
    pagination: { total: '共 {total} 条', perPage: '{size}条/页', prev: '上一页', next: '下一页' },
    migration: {
      notFoundTitle: '页面不存在',
      notFoundBody: '这个地址没有对应的页面。',
      backToTasks: '返回任务列表',
    },
  },
  en: {
    workersView: { status: 'Status', load: 'Tasks', heartbeat: 'Heartbeat' },
    pagination: { total: 'Total {total}', perPage: '{size}/page', prev: 'Previous', next: 'Next' },
    migration: {
      notFoundTitle: 'Page not found',
      notFoundBody: 'Nothing lives at this address.',
      backToTasks: 'Back to tasks',
    },
  },
}

const catalogs: Record<Locale, unknown> = {
  'zh-CN': mergeCatalog(zhCN, consoleOnly['zh-CN']),
  en: mergeCatalog(en, consoleOnly.en),
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
    // vue-i18n literal interpolation: {'@@server_uuid'} renders @@server_uuid.
    return msg.replace(
      /\{'([^']*)'\}|\{(\w+)\}/g,
      (m, literal: string | undefined, name: string) => {
        if (literal !== undefined) return literal
        return params && name in params ? String(params[name]) : m
      },
    )
  }
}

/**
 * Translate a key built from server data (a state, status or reason). When the
 * catalog has no such key, return the fallback instead of the raw key, so an
 * unknown value never shows as "state.FOO".
 */
export function tKnown(
  t: TFunction,
  key: string,
  fallback: string,
  params?: Record<string, unknown>,
) {
  const text = t(key, params)
  return text === key ? fallback : text
}

const I18nContext = createContext<{ locale: Locale; t: TFunction }>({
  locale: 'zh-CN',
  t: makeT('zh-CN'),
})

export function I18nProvider({ children }: { children: ReactNode }) {
  const locale = getLocale()
  // Stable across renders so hooks can depend on t without refetch loops.
  const value = useMemo(() => ({ locale, t: makeT(locale) }), [locale])
  return <I18nContext.Provider value={value}>{children}</I18nContext.Provider>
}

export function useI18n() {
  return useContext(I18nContext)
}
