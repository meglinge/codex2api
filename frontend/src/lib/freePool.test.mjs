import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  freePoolErrorKey,
  freePoolQuery,
  newFreePoolAccountDraft,
  parseFreePoolSourceID,
  validateFreePoolAccount,
} from './freePool.ts'

function validDraft(patch = {}) {
  return { ...newFreePoolAccountDraft(), name: ' source-1 ', credentials: '{"access_token":"fixture-only"}', ...patch }
}

test('free pool import defaults to disabled and trims non-secret fields', () => {
  const result = validateFreePoolAccount(validDraft())
  assert.equal(result.ok, true)
  assert.deepEqual(result.value, {
    name: 'source-1',
    status: 'disabled',
    proxy_url: '',
    credentials: { access_token: 'fixture-only' },
  })
})

test('credentials must be a non-empty object without imposing a token schema', () => {
  for (const credentials of ['', '{', '{}', '[]', 'null', '"token"', '1']) {
    const result = validateFreePoolAccount(validDraft({ credentials }))
    assert.equal(result.ok, false)
    assert.equal(result.errorKey, 'freePool.validation.credentials')
  }
  assert.equal(validateFreePoolAccount(validDraft({ credentials: '{"custom_field":{"value":"fixture"}}' })).ok, true)
})

test('name and credentials limits use UTF-8 bytes', () => {
  assert.equal(validateFreePoolAccount(validDraft({ name: '界'.repeat(42) })).ok, true)
  assert.equal(validateFreePoolAccount(validDraft({ name: '界'.repeat(43) })).errorKey, 'freePool.validation.name')
  const oversized = JSON.stringify({ token: '界'.repeat(22000) })
  assert.equal(validateFreePoolAccount(validDraft({ credentials: oversized })).errorKey, 'freePool.validation.credentialsSize')
})

test('proxy accepts supported protocols but rejects invalid URLs', () => {
  for (const proxy_url of ['', 'http://localhost:8080', 'https://localhost:8443', 'socks5://user:password@localhost:1080', 'socks5h://localhost:1080']) {
    assert.equal(validateFreePoolAccount(validDraft({ proxy_url })).ok, true, proxy_url)
  }
  for (const proxy_url of ['localhost:8080', 'file:///tmp/test', 'https://']) {
    assert.equal(validateFreePoolAccount(validDraft({ proxy_url })).errorKey, 'freePool.validation.proxy')
  }
})

test('query uses cursor pagination and omits empty filters', () => {
  assert.equal(freePoolQuery(), 'limit=50')
  assert.equal(freePoolQuery({ limit: 25, before_id: 100, status: 'candidate', source_account_id: 7 }), 'limit=25&before_id=100&status=candidate&source_account_id=7')
  assert.equal(freePoolQuery({ before_id: 0, status: '', source_account_id: 0 }), 'limit=50')
})

test('source filter rejects fractional, exponential and unsafe IDs', () => {
  assert.deepEqual(parseFreePoolSourceID(''), { ok: true, value: 0 })
  assert.deepEqual(parseFreePoolSourceID(' 42 '), { ok: true, value: 42 })
  for (const input of ['0', '-1', '1.5', '1e3', '9007199254740992']) {
    assert.equal(parseFreePoolSourceID(input).ok, false)
  }
})

test('errors are mapped by status rather than server text', () => {
  assert.equal(freePoolErrorKey(501), 'freePool.errors.unsupported')
  assert.equal(freePoolErrorKey(401), 'freePool.errors.authRequired')
  assert.equal(freePoolErrorKey(404), 'freePool.errors.notFound')
  assert.equal(freePoolErrorKey(400), 'freePool.errors.invalidRequest')
  assert.equal(freePoolErrorKey(500), 'freePool.errors.requestFailed')
  assert.equal(freePoolErrorKey(), 'freePool.errors.requestFailed')
})

function leafKeys(value, prefix = '') {
  return Object.entries(value).flatMap(([key, item]) => {
    const path = prefix ? `${prefix}.${key}` : key
    return typeof item === 'string' ? [path] : leafKeys(item, path)
  }).sort()
}

test('all three locales contain the same free-pool keys and navigation label', () => {
  const locales = ['zh', 'en', 'zh-TW'].map((language) => JSON.parse(readFileSync(new URL(`../locales/${language}.json`, import.meta.url), 'utf8')))
  for (const locale of locales) {
    assert.equal(typeof locale.nav.freePool, 'string')
    assert.deepEqual(leafKeys(locale.freePool), leafKeys(locales[0].freePool))
  }
})
