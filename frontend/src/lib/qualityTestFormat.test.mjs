import assert from 'node:assert/strict'
import test from 'node:test'
import { formatQualitySource, QUALITY_TEST_FORMAT_LIMIT } from './qualityTestFormat.ts'

test('generated markup is re-indented, including embedded css and svg, and bad input passes through', async () => {
  const flat = '<!DOCTYPE html><html><head><style>.a{fill:red;stroke:none}</style></head><body><svg viewBox="0 0 10 10"><g id="x"><circle r="1"/><circle r="2"/></g></svg></body></html>'
  const formatted = await formatQualitySource(flat)
  assert.ok(formatted.includes('\n    <style>\n      .a {\n        fill: red;\n        stroke: none;\n      }\n    </style>'), formatted)
  assert.ok(formatted.includes('<g id="x">\n'), formatted)
  assert.ok(formatted.includes('<circle r="1" />'), formatted)
  assert.equal(await formatQualitySource(''), '')
  const huge = '<p>' + 'x'.repeat(QUALITY_TEST_FORMAT_LIMIT) + '</p>'
  assert.equal(await formatQualitySource(huge), huge)
})
