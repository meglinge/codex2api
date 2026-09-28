import assert from 'node:assert/strict'
import test from 'node:test'

import { ADMIN_AUTH_REQUIRED_EVENT, AdminAPIError, api } from '../api.ts'
import { freePoolErrorKey } from './freePool.ts'

test('free pool API reuses admin auth, cursor queries and error semantics', async (t) => {
  const originals = new Map(['localStorage', 'window'].map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]))
  const storage = new Map([['admin_key', 'fixture-admin-key']])
  const events = []
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, String(value)),
      removeItem: (key) => storage.delete(key),
    },
  })
  Object.defineProperty(globalThis, 'window', {
    configurable: true,
    value: { dispatchEvent: (event) => { events.push(event.type); return true } },
  })
  t.after(() => {
    for (const [key, descriptor] of originals) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor)
      else Reflect.deleteProperty(globalThis, key)
    }
  })

  let responseStatus = 200
  let responseBody = { items: [], next_before_id: null }
  const calls = []
  t.mock.method(globalThis, 'fetch', async (url, options) => {
    calls.push({ url: String(url), method: options.method ?? 'GET', headers: new Headers(options.headers), body: options.body, signal: options.signal })
    if (responseStatus === 204) return new Response(null, { status: 204 })
    return new Response(JSON.stringify(responseBody), { status: responseStatus, headers: { 'Content-Type': 'application/json' } })
  })

  const signal = new AbortController().signal
  await api.listFreePoolAccounts({ before_id: 30, status: 'disabled' }, signal)
  assert.equal(calls.at(-1).url, '/api/admin/free-pool/accounts?limit=50&before_id=30&status=disabled')
  assert.equal(calls.at(-1).signal, signal)

  responseStatus = 201
  responseBody = { id: 9 }
  const input = { name: 'fixture-source', status: 'disabled', proxy_url: '', credentials: { access_token: 'fixture-only' } }
  assert.deepEqual(await api.createFreePoolAccount(input), { id: 9 })
  assert.equal(calls.at(-1).method, 'POST')
  assert.deepEqual(JSON.parse(calls.at(-1).body), input)

  responseStatus = 200
  responseBody = { id: 9, status: 'active' }
  await api.setFreePoolAccountStatus(9, 'active')
  assert.equal(calls.at(-1).method, 'PATCH')

  responseStatus = 204
  assert.equal(await api.deleteFreePoolAccount(9), undefined)
  assert.equal(calls.at(-1).method, 'DELETE')

  responseStatus = 200
  responseBody = { items: [], next_before_id: null }
  await api.listFreePoolTickets({ before_id: 80, status: 'candidate', source_account_id: 9 }, signal)
  const ticketURL = new URL(calls.at(-1).url, 'http://unit.test')
  assert.equal(ticketURL.pathname, '/api/admin/free-pool/tickets')
  assert.deepEqual(Object.fromEntries(ticketURL.searchParams), { limit: '50', before_id: '80', status: 'candidate', source_account_id: '9' })

  responseBody = { ticket_id: 12, total: 0, items: [] }
  await api.getFreePoolProbeSummary(12, signal)
  assert.equal(calls.at(-1).url, '/api/admin/free-pool/tickets/12/probes')
  for (const call of calls) assert.equal(call.headers.get('X-Admin-Key'), 'fixture-admin-key')

  responseStatus = 501
  responseBody = { error: 'server-body-not-for-ui' }
  await assert.rejects(api.listFreePoolAccounts(), (error) => {
    assert.ok(error instanceof AdminAPIError)
    assert.equal(error.status, 501)
    assert.equal(freePoolErrorKey(error.status), 'freePool.errors.unsupported')
    return true
  })

  responseStatus = 401
  await assert.rejects(api.listFreePoolTickets(), (error) => error instanceof AdminAPIError && error.status === 401)
  assert.equal(storage.has('admin_key'), false)
  assert.ok(events.includes(ADMIN_AUTH_REQUIRED_EVENT))
})
