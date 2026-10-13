// Every literal message key either console uses must exist in both catalogs,
// so a missing key fails here instead of showing as raw text on screen.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { makeT } from '@/lib/i18n'

function files(dir: string, ext: RegExp, out: string[] = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) files(p, ext, out)
    else if (ext.test(name) && !name.includes('.test.')) out.push(p)
  }
  return out
}

function keys(dir: string, ext: RegExp, re: RegExp) {
  const found = new Set<string>()
  for (const f of files(dir, ext)) {
    for (const m of readFileSync(f, 'utf8').matchAll(re)) found.add(m[1])
  }
  return [...found].sort()
}

const KEY = String.raw`([a-zA-Z]+(?:\.[a-zA-Z0-9_]+)+)`
const sources = {
  console: keys(
    join(__dirname, '..'),
    /\.(ts|tsx)$/,
    new RegExp(String.raw`\bt\(\s*'${KEY}'`, 'g'),
  ),
  classic: keys(
    join(__dirname, '../../../frontend/src'),
    /\.(vue|js)$/,
    new RegExp(String.raw`\$?\bt\(\s*["']${KEY}["']`, 'g'),
  ),
}

describe.each(['en', 'zh-CN'] as const)('%s catalog', (locale) => {
  const t = makeT(locale)
  it.each(Object.entries(sources))('has every key the %s console uses', (_, used) => {
    expect(used.length).toBeGreaterThan(20)
    expect(used.filter((k) => t(k) === k)).toEqual([])
  })
})
