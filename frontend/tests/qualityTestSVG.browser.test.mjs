import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { readFile } from 'node:fs/promises'
import { chromium } from 'playwright'
import { qualityTestPreviewDocument } from '../src/lib/qualityTest.ts'
import { qualityTestSVGExportScript, requestQualityTestSVG, validateQualityTestSVG, QUALITY_TEST_SVG_LIMIT, QUALITY_TEST_SVG_TIMEOUT } from '../src/lib/qualityTestSVG.ts'

let browser
before(async () => { browser = await chromium.launch({ headless: true }) })
after(async () => { await browser?.close() })

const fixture = `<!doctype html><html><head><style>
  :root { --bird: rgb(18, 90, 66) }
  body .scene .feather { fill: var(--bird) }
  .scene { width:100%; height:auto }
  @media(max-width:700px) { .scene { width:166%;margin-left:-33%;margin-top:85px;max-width:none } }
  .turn { animation: turn 4s linear infinite; transform-origin:20px 20px; transform-box:view-box }
  @keyframes turn { to { transform:rotate(360deg) } }
  .label::before { content:"A & B" }
</style></head><body>
<svg width="20" height="20"><path d="M0 0h10v10z"/></svg>
<svg class="scene" viewBox="0 0 640 320" xmlns="http://www.w3.org/2000/svg">
  <defs><linearGradient id="sky"><stop stop-color="#abd"/><stop offset="1" stop-color="#def"/></linearGradient></defs>
  <rect width="640" height="320" fill="url(#sky)"/>
  <path id="leg" d="" class="feather"/><g id="foot"><rect width="12" height="8"/></g>
  <g class="turn" id="wheel"><path d="M0 20h40" stroke="red"/></g>
  <circle id="pulse" cx="100" cy="50" r="10"><animate attributeName="r" from="10" to="30" dur="4s" repeatCount="indefinite"/></circle>
  <g id="travel"><circle r="3"/><animateMotion path="M0,0 L100,0" dur="4s" repeatCount="indefinite"/></g>
  <text x="10" y="300">A &amp; B &lt; coast</text>
</svg>
<button id="play">Pause</button><script>
const leg=document.getElementById('leg'),foot=document.getElementById('foot');
leg.setAttribute('d','M200 120 L240 170 L220 220');
foot.setAttribute('transform','translate(220 220) rotate(12)');
const extra=document.createElementNS('http://www.w3.org/2000/svg','circle');extra.id='generated';extra.setAttribute('r','5');extra.setAttribute('cx','150');document.querySelector('.scene').append(extra);
document.getElementById('play').addEventListener('click',()=>{});
</script></body></html>`

async function preview(page, html, width = 800) {
  await page.setViewportSize({ width, height: 800 })
  await page.setContent('<iframe sandbox="allow-scripts" style="border:0;width:100%;height:700px"></iframe>')
  const markup = qualityTestPreviewDocument(html, qualityTestSVGExportScript('test-preview'))
  return page.evaluate(({ markup, requestSource, validateSource, limit, timeout }) => {
    const frame = document.querySelector('iframe')
    window.requestSVG = new Function('QUALITY_TEST_SVG_LIMIT', 'QUALITY_TEST_SVG_TIMEOUT', `return (${requestSource})`)(limit, timeout)
    window.validateSVG = new Function('QUALITY_TEST_SVG_LIMIT', `return (${validateSource})`)(limit)
    window.exportSVG = async () => window.validateSVG(await window.requestSVG(frame.contentWindow, 'test-preview', new AbortController().signal))
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('preview did not become ready')), 8000)
      const receive = event => {
        if (event.source !== frame.contentWindow || event.data?.type !== 'quality-test-svg-ready') return
        window.removeEventListener('message', receive)
        clearTimeout(timer)
        resolve(event.data.hasSVG)
      }
      window.addEventListener('message', receive)
      frame.srcdoc = markup
    })
  }, { markup, requestSource: requestQualityTestSVG.toString(), validateSource: validateQualityTestSVG.toString(), limit: QUALITY_TEST_SVG_LIMIT, timeout: QUALITY_TEST_SVG_TIMEOUT })
}

async function openSVG(page, svg, width = 390) {
  await page.setViewportSize({ width, height: 800 })
  await page.goto(`data:image/svg+xml;base64,${Buffer.from(svg).toString('base64')}`)
  assert.equal(await page.locator('parsererror').count(), 0)
}

test('sandbox snapshot retains runtime geometry, scoped CSS and frozen CSS/SMIL animation', async () => {
  const page = await browser.newPage()
  try {
    assert.equal(await preview(page, fixture), true)
    const frame = page.frames()[1]
    await frame.evaluate(() => {
      document.getAnimations().forEach(animation => { animation.pause(); animation.currentTime = 1000 })
      const svg = document.querySelector('.scene'); svg.pauseAnimations(); svg.setCurrentTime(1)
    })
    const expected = await frame.evaluate(() => ({ radius: document.getElementById('pulse').r.animVal.value, motion: document.getElementById('travel').getCTM().e }))
    const svg = await page.evaluate(() => window.exportSVG())
    assert.ok(svg.includes('M200 120 L240 170 L220 220'))
    assert.ok(svg.includes('id="generated"'))
    assert.ok(svg.includes('A &amp; B &lt; coast'))
    assert.ok(!/<(?:script|style|animate|set)[\s>]/.test(svg))
    assert.ok(!svg.includes('166%'))
    // Capturing must not alter the original preview's animation state.
    assert.equal(await frame.evaluate(() => document.querySelector('.scene').animationsPaused()), true)
    await openSVG(page, svg)
    const result = await page.evaluate(() => {
      const svg = document.documentElement, style = getComputedStyle(document.getElementById('wheel'))
      return { width: svg.getBoundingClientRect().width, scrollWidth: svg.scrollWidth, leg: document.getElementById('leg').getAttribute('d'), fill: getComputedStyle(document.getElementById('leg')).fill, foot: getComputedStyle(document.getElementById('foot')).transform, turn: style.transform, radius: document.getElementById('pulse').r.animVal.value, motion: document.getElementById('travel').getCTM().e, scale: svg.getCTM().a, animations: document.getAnimations().length }
    })
    assert.equal(result.width, 390)
    assert.equal(result.scrollWidth, 390)
    assert.equal(result.fill, 'rgb(18, 90, 66)')
    assert.ok(result.foot.includes('220'))
    assert.match(result.turn, /^matrix\(0, 1, -1, 0, 40, 0\)$/)
    assert.equal(result.radius, expected.radius)
    assert.ok(Math.abs(result.motion / result.scale - 25) < 0.01)
    assert.equal(result.animations, 0)
    const before = await page.locator('svg').getAttribute('style')
    await page.waitForTimeout(100)
    assert.equal(await page.locator('svg').getAttribute('style'), before)
    await page.goto('about:blank')
    await page.setContent('<img alt="snapshot" style="width:390px">')
    const image = await page.evaluate(async svg => {
      const image = document.querySelector('img')
      image.src = URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml' }))
      await image.decode()
      return { width: image.naturalWidth, height: image.naturalHeight }
    }, svg)
    assert.deepEqual(image, { width: 640, height: 320 })
  } finally { await page.close() }
})

test('mobile page rules cannot stretch the standalone SVG viewport', async () => {
  const page = await browser.newPage()
  try {
    await preview(page, fixture, 390)
    const svg = await page.evaluate(() => window.exportSVG())
    await openSVG(page, svg)
    assert.deepEqual(await page.evaluate(() => ({ width: document.documentElement.getBoundingClientRect().width, scroll: document.documentElement.scrollWidth })), { width: 390, scroll: 390 })
  } finally { await page.close() }
})

test('Canvas and HTML-dependent SVGs produce explicit export errors', async () => {
  const page = await browser.newPage()
  try {
    assert.equal(await preview(page, '<canvas width="640" height="320"></canvas>'), false)
    await assert.rejects(page.evaluate(() => window.exportSVG()), /noSVG/)
    await preview(page, '<svg width="640" height="320"><foreignObject width="50" height="50"><div>HTML</div></foreignObject></svg>')
    await assert.rejects(page.evaluate(() => window.exportSVG()), /unsupported/)
    await preview(page, '<svg width="640" height="320"><image href="https://example.invalid/a.png"/></svg>')
    await assert.rejects(page.evaluate(() => window.exportSVG()), /unsupported/)
    await preview(page, '<svg width="0" height="0"><defs><path id="outside" d="M0 0h10v10z"/></defs></svg><svg width="640" height="320"><use href="#outside"/></svg>')
    await assert.rejects(page.evaluate(() => window.exportSVG()), /unsupported/)
  } finally { await page.close() }
})

test('parent rejects malformed XML, active content and non-local references', async () => {
  const page = await browser.newPage()
  try {
    await preview(page, fixture)
    for (const fragment of ['<text>A & B</text>', '<script>alert(1)</script>', '<g onclick="alert(1)"/>', '<foreignObject/>', '<use href="https://example.invalid/s.svg#x"/>', '<rect fill="url(https://example.invalid/s.svg#x)"/>', '<rect style="fill:url(https://example.invalid/s.svg#x)"/>', '<style>@import "https://example.invalid/x.css"</style>']) {
      await assert.rejects(page.evaluate(value => window.validateSVG(value), `<svg xmlns="http://www.w3.org/2000/svg">${fragment}</svg>`), /invalidSVG/)
    }
  } finally { await page.close() }
})

test('SVG created after load becomes exportable and oversized scenes fail before serialization', async () => {
  const page = await browser.newPage()
  try {
    assert.equal(await preview(page, `<script>setTimeout(()=>{document.body.insertAdjacentHTML('beforeend','<svg width="400" height="200"><circle r="20"/></svg>')},500)</script>`), false)
    await page.evaluate(() => new Promise(resolve => window.addEventListener('message', event => { if (event.data?.type === 'quality-test-svg-ready' && event.data.hasSVG) resolve() })))
    assert.ok((await page.evaluate(() => window.exportSVG())).includes('<circle'))
    await preview(page, '<svg width="400" height="200">' + '<circle r="1"/>'.repeat(10001) + '</svg>')
    await assert.rejects(page.evaluate(() => window.exportSVG()), /tooLarge/)
  } finally { await page.close() }
})

test('locally supplied HTML sample exports initialized pelican legs and feet', { skip: !process.env.QUALITY_TEST_SVG_SAMPLE }, async () => {
  const page = await browser.newPage()
  try {
    await preview(page, await readFile(process.env.QUALITY_TEST_SVG_SAMPLE, 'utf8'))
    const svg = await page.evaluate(() => window.exportSVG())
    await openSVG(page, svg)
    for (const id of ['frontLeg', 'backLeg']) assert.ok((await page.locator(`#${id}`).getAttribute('d')).length > 20)
    for (const id of ['frontFoot', 'backFoot']) assert.ok(await page.locator(`#${id}`).evaluate(element => element.getBoundingClientRect().y > 100))
  } finally { await page.close() }
})
