/**
 * Feature-local locale guard.
 *
 * Mirrors the repo-wide `check:i18n` rules for this feature so the payload can be
 * handed to the shared locale bundles without surprises: both languages must
 * carry exactly the same leaf keys, every leaf must be a non-empty string, and
 * every literal `admin.errorDiagnostics.*` key used by the feature components must
 * exist and be reachable in the shared bundles.
 *
 * The payload in ../locale is spread into the shared `admin` namespace, so its
 * own keys are `errorDiagnostics.*` while components address them as
 * `admin.errorDiagnostics.*`.
 */
import { describe, expect, it } from 'vitest'
import sharedEn from '@/i18n/locales/en'
import sharedZh from '@/i18n/locales/zh'
import { errorDiagnosticsEn, errorDiagnosticsZh } from '../locale'

const ADMIN_PREFIX = 'admin.'

type LocaleValue = Record<string, unknown>

function flattenLeafKeys(value: unknown, prefix = ''): string[] {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return prefix ? [prefix] : []
  }

  return Object.entries(value as LocaleValue).flatMap(([key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key
    return flattenLeafKeys(child, path)
  })
}

function readLeaf(messages: unknown, key: string): unknown {
  return key.split('.').reduce<unknown>((current, segment) => {
    if (current === null || typeof current !== 'object') return undefined
    return (current as LocaleValue)[segment]
  }, messages)
}

/** Literal `t('...')` keys used by the feature components, without the namespace. */
function usedKeys(): string[] {
  const sources = import.meta.glob('../../error-diagnostics/**/*.{ts,vue}', {
    query: '?raw',
    import: 'default',
    eager: true,
  }) as Record<string, string>

  const keys = new Set<string>()
  for (const [path, source] of Object.entries(sources)) {
    if (path.includes('/__tests__/')) continue
    // Dynamic template keys are covered by the label maps in labels.ts.
    for (const match of source.matchAll(/\bt\s*\(\s*(['"])([^'"\r\n]+)\1/g)) {
      if (match[2].startsWith(ADMIN_PREFIX)) {
        keys.add(match[2].slice(ADMIN_PREFIX.length))
      }
    }
  }
  return [...keys].sort()
}

describe('error diagnostics locale', () => {
  const enKeys = flattenLeafKeys(errorDiagnosticsEn).sort()
  const zhKeys = flattenLeafKeys(errorDiagnosticsZh).sort()
  const literalKeys = usedKeys()

  it('keeps English and Chinese schemas identical', () => {
    expect(enKeys.filter((key) => !zhKeys.includes(key))).toEqual([])
    expect(zhKeys.filter((key) => !enKeys.includes(key))).toEqual([])
  })

  it('contains a non-empty message for every leaf', () => {
    for (const [locale, messages] of Object.entries({ en: errorDiagnosticsEn, zh: errorDiagnosticsZh })) {
      const empty = flattenLeafKeys(messages).filter((key) => {
        const value = readLeaf(messages, key)
        return typeof value !== 'string' || value.trim() === ''
      })
      expect(empty, `${locale} has empty leaves`).toEqual([])
    }
  })

  it('defines every literal key the feature components use', () => {
    expect(literalKeys.filter((key) => !enKeys.includes(key))).toEqual([])
  })

  it('is reachable from the shared locale bundles', () => {
    // Wire-up guard: the payload is only served once
    // i18n/locales/{en,zh}/admin/index.ts imports and spreads it into `admin`.
    const missingEn = enKeys.filter((key) => readLeaf(sharedEn, `${ADMIN_PREFIX}${key}`) === undefined)
    const missingZh = zhKeys.filter((key) => readLeaf(sharedZh, `${ADMIN_PREFIX}${key}`) === undefined)
    expect(missingEn, 'en bundle is missing diagnostics keys').toEqual([])
    expect(missingZh, 'zh bundle is missing diagnostics keys').toEqual([])
  })

  it('covers every body state and reason the storage layer can persist', () => {
    // Guards against the closed sets drifting apart from the copy.
    const bodyStateKeys = enKeys.filter((key) => key.startsWith('errorDiagnostics.bodyStates.'))
    const reasonKeys = enKeys.filter((key) => key.startsWith('errorDiagnostics.reasons.'))
    expect(bodyStateKeys).toHaveLength(6) // 5 persisted states + unknown
    expect(reasonKeys).toHaveLength(10) // 9 persisted reasons + unknown
  })
})
