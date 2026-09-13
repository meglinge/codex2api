import assert from 'node:assert/strict'
import test from 'node:test'
import { formatImageStudioQuota } from './imageStudioQuota.ts'

test('quota formatting distinguishes unavailable, zero, and tiny positive balances', () => {
  assert.equal(formatImageStudioQuota(null), '—')
  assert.equal(formatImageStudioQuota(undefined), '—')
  assert.equal(formatImageStudioQuota(Number.NaN), '—')
  assert.equal(formatImageStudioQuota(0), '$0.00')
  assert.equal(formatImageStudioQuota(-0.5), '$0.00')
  assert.equal(formatImageStudioQuota(0.000001), '< $0.0001')
  assert.equal(formatImageStudioQuota(0.0001), '$0.0001')
  assert.equal(formatImageStudioQuota(18.6), '$18.60')
  assert.equal(formatImageStudioQuota(1234.5678), '$1,234.5678')
})
