import assert from 'node:assert/strict'
import test from 'node:test'
import { requestQualityTestSVG, qualityTestSVGExportScript } from './qualityTestSVG.ts'
import { qualityTestPreviewDocument } from './qualityTest.ts'

function transport() {
  const host = new EventTarget()
  const requests = []
  const frame = { postMessage: (data) => requests.push(data) }
  const controller = new AbortController()
  const respond = (patch = {}, source = frame) => {
    const event = new Event('message')
    Object.assign(event, { source, data: { ...requests.at(-1), type: 'quality-test-svg-result', svg: '<svg/>', ...patch } })
    host.dispatchEvent(event)
  }
  return { host, requests, frame, controller, respond }
}

test('SVG responses must match the frame, preview generation and individual request', async () => {
  const t = transport()
  const pending = requestQualityTestSVG(t.frame, 'preview-a', t.controller.signal, t.host)
  let completed = false
  void pending.then(() => { completed = true })
  t.respond({}, {})
  t.respond({ previewID: 'preview-b' })
  t.respond({ requestID: 'old-request' })
  await Promise.resolve()
  assert.equal(completed, false)
  t.respond({ svg: '<svg id="current"/>' })
  assert.equal(await pending, '<svg id="current"/>')
})

test('navigation or replay cancels an in-flight SVG export, including late replies', async () => {
  const t = transport()
  const pending = requestQualityTestSVG(t.frame, 'preview-a', t.controller.signal, t.host)
  t.controller.abort()
  t.respond()
  await assert.rejects(pending, /cancelled/)
  await assert.rejects(requestQualityTestSVG(t.frame, 'preview-b', t.controller.signal, t.host), /cancelled/)
  assert.equal(t.requests.length, 1)
})

test('unresponsive previews time out and malformed or unsupported exports fail', async () => {
  const t = transport()
  await assert.rejects(requestQualityTestSVG(t.frame, 'preview-a', t.controller.signal, t.host, 10), /timeout/)
  const unsupported = requestQualityTestSVG(t.frame, 'preview-a', t.controller.signal, t.host)
  t.respond({ error: 'unsupported' })
  await assert.rejects(unsupported, /unsupported/)
  const malformed = requestQualityTestSVG(t.frame, 'preview-a', t.controller.signal, t.host)
  t.respond({ svg: 123 })
  await assert.rejects(malformed, /invalidSVG/)
})

test('export bridge is appended within the existing preview policy and safely quotes its generation ID', () => {
  const bridge = qualityTestSVGExportScript('</script><script>bad()</script>')
  assert.equal((bridge.match(/<script>/g) ?? []).length, 1)
  const preview = qualityTestPreviewDocument('<html><body>test</body></html>', bridge)
  assert.ok(preview.indexOf('Content-Security-Policy') < preview.indexOf(bridge))
  assert.ok(preview.includes("connect-src 'none'"))
  assert.ok(!preview.includes('allow-same-origin'))
})
