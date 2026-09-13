export const QUALITY_TEST_SVG_LIMIT = 8 * 1024 * 1024
export const QUALITY_TEST_SVG_TIMEOUT = 8000

// This function is serialized into the opaque-origin preview. Keep all browser
// helpers inside it: the generated document cannot import the admin bundle.
export function captureQualityTestSVG(maxBytes: number): string {
  const namespace = 'http://www.w3.org/2000/svg'
  const candidates = [...document.querySelectorAll('svg')].filter((svg) => !svg.ownerSVGElement)
    .map((svg) => ({ svg, rect: svg.getBoundingClientRect(), style: getComputedStyle(svg) }))
    .filter(({ rect, style }) => rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden')
    .sort((a, b) => b.rect.width * b.rect.height - a.rect.width * a.rect.height)
  const source = candidates[0]?.svg
  if (!source) throw new Error('noSVG')
  // HTML inside foreignObject and resources outside this SVG need their original
  // document. Report that limitation instead of silently producing a broken file.
  if (source.querySelector('foreignObject')) throw new Error('unsupported')
  const originals = [source, ...source.querySelectorAll('*')]
  if (originals.length > 10000) throw new Error('tooLarge')
  const clone = source.cloneNode(true) as SVGSVGElement
  const copies = [clone, ...clone.querySelectorAll('*')]
  const properties = [
    'color', 'color-interpolation', 'color-interpolation-filters', 'display', 'visibility',
    'fill', 'fill-opacity', 'fill-rule', 'opacity', 'stroke', 'stroke-width', 'stroke-opacity',
    'stroke-linecap', 'stroke-linejoin', 'stroke-miterlimit', 'stroke-dasharray', 'stroke-dashoffset',
    'paint-order', 'vector-effect', 'clip-path', 'clip-rule', 'mask', 'filter',
    'marker-start', 'marker-mid', 'marker-end', 'stop-color', 'stop-opacity', 'flood-color', 'flood-opacity',
    'lighting-color', 'font-family', 'font-size', 'font-style', 'font-weight', 'font-stretch',
    'letter-spacing', 'word-spacing', 'text-anchor', 'dominant-baseline', 'alignment-baseline',
    'baseline-shift', 'text-decoration', 'white-space', 'writing-mode', 'direction',
    'shape-rendering', 'text-rendering', 'image-rendering', 'overflow',
    'transform', 'transform-origin', 'transform-box', 'd', 'cx', 'cy', 'r', 'rx', 'ry', 'x', 'y',
  ]
  const localReference = (value: string): string => {
    if (!value || /^data:image\/(?:png|jpeg|gif|webp|avif);base64,/i.test(value)) return value
    const url = new URL(value, document.baseURI)
    if (url.hash && (value.startsWith('#') || url.href.split('#')[0] === new URL(document.baseURI).href.split('#')[0])) {
      const target = document.getElementById(decodeURIComponent(url.hash.slice(1)))
      if (target && source.contains(target)) return url.hash
    }
    throw new Error('unsupported')
  }
  const localURLs = (value: string) => value.replace(/url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*?))\s*\)/gi,
    (_, double: string, single: string, bare: string) => `url("${localReference(double ?? single ?? bare).replace(/"/g, '%22')}")`)
  const matrixText = (matrix: DOMMatrix) => `matrix(${[matrix.a, matrix.b, matrix.c, matrix.d, matrix.e, matrix.f].join(',')})`

  originals.forEach((original, index) => {
    const copy = copies[index] as SVGElement
    if (original.namespaceURI !== namespace) throw new Error('unsupported')
    const computed = getComputedStyle(original)
    copy.removeAttribute('style')
    for (const attribute of [...copy.attributes]) {
      if (/^on/i.test(attribute.localName) || attribute.localName === 'base') copy.removeAttributeNode(attribute)
      else if (attribute.localName === 'href') {
        if (copy.localName === 'a') copy.removeAttributeNode(attribute)
        else attribute.value = localReference(attribute.value)
      } else if (/url\(/i.test(attribute.value)) attribute.value = localURLs(attribute.value)
    }
    for (const property of properties) {
      const value = computed.getPropertyValue(property)
      if (value) copy.style.setProperty(property, localURLs(value))
    }
    // Resolve lengths while the original viewport still exists. animVal also
    // captures SMIL geometry, which cloneNode alone only copies at its base value.
    for (const name of ['x', 'y', 'width', 'height', 'cx', 'cy', 'r', 'rx', 'ry', 'x1', 'x2', 'y1', 'y2']) {
      const animated = (original as unknown as Record<string, { animVal?: { value?: number } }>)[name]
      const value = animated?.animVal?.value
      if (typeof value === 'number' && Number.isFinite(value)) {
        copy.setAttribute(name, String(value))
        // CSS geometry overrides SVG attributes; retain it where it is present.
        const css = computed.getPropertyValue(name)
        if (css && css !== 'auto') copy.style.setProperty(name, css)
      }
    }
    const points = (original as SVGPolylineElement).animatedPoints
    if (points?.numberOfItems) copy.setAttribute('points', Array.from({ length: points.numberOfItems }, (_, i) => {
      const point = points.getItem(i)
      return `${point.x},${point.y}`
    }).join(' '))
    // Include animateMotion and CSS/SMIL transforms in the current local matrix.
    // Nested svg elements already apply their own viewport transform, so retain
    // their computed transform instead of applying that viewport twice.
    const graphics = original as SVGGraphicsElement
    const parent = original.parentElement as unknown as SVGGraphicsElement | null
    if (original !== source && original.localName !== 'svg' && graphics.getScreenCTM && parent?.getScreenCTM) {
      const ownMatrix = graphics.getScreenCTM(), parentMatrix = parent.getScreenCTM()
      if (ownMatrix && parentMatrix) {
        const matrix = parentMatrix.inverse().multiply(ownMatrix)
        if ([matrix.a, matrix.b, matrix.c, matrix.d, matrix.e, matrix.f].every(Number.isFinite)) {
          copy.style.setProperty('transform', matrixText(matrix))
          copy.style.setProperty('transform-origin', '0px 0px')
          copy.style.setProperty('transform-box', 'view-box')
        }
      }
    }
    for (const name of ['gradientTransform', 'patternTransform']) {
      const transforms = (original as unknown as Record<string, SVGAnimatedTransformList>)[name]?.animVal
      if (transforms?.numberOfItems) {
        let matrix = new DOMMatrix()
        for (let i = 0; i < transforms.numberOfItems; i++) matrix = matrix.multiply(transforms.getItem(i).matrix)
        copy.setAttribute(name, matrixText(matrix))
      }
    }
  })
  clone.querySelectorAll('script,style,animate,animateTransform,animateMotion,set,discard').forEach((node) => node.remove())
  const box = source.viewBox.animVal
  const width = box.width > 0 ? box.width : source.width.animVal.value || candidates[0].rect.width
  const height = box.height > 0 ? box.height : source.height.animVal.value || candidates[0].rect.height
  if (![width, height].every((value) => Number.isFinite(value) && value > 0 && value <= 32768)) throw new Error('unsupported')
  clone.setAttribute('xmlns', namespace)
  clone.setAttribute('viewBox', box.width > 0 && box.height > 0 ? `${box.x} ${box.y} ${width} ${height}` : `0 0 ${width} ${height}`)
  clone.setAttribute('width', String(width))
  clone.setAttribute('height', String(height))
  clone.style.setProperty('width', '100%')
  clone.style.setProperty('height', 'auto')
  clone.style.setProperty('max-width', '100%')
  clone.style.setProperty('display', 'block')
  clone.style.setProperty('margin', '0')
  clone.style.setProperty('background-color', getComputedStyle(source).backgroundColor)
  const serialized = `<?xml version="1.0" encoding="UTF-8"?>\n${new XMLSerializer().serializeToString(clone)}`
  if (new TextEncoder().encode(serialized).length > maxBytes) throw new Error('tooLarge')
  return serialized
}

function installQualityTestSVGBridge(previewID: string, capture: (limit: number) => string, limit: number) {
  let loaded = false, available: boolean | undefined, timer: ReturnType<typeof setTimeout> | undefined
  const report = () => {
    if (!loaded) return
    const hasSVG = Boolean(document.querySelector('svg'))
    if (available === hasSVG) return
    available = hasSVG
    parent.postMessage({ type: 'quality-test-svg-ready', previewID, hasSVG }, '*')
  }
  const ready = () => requestAnimationFrame(() => requestAnimationFrame(() => { loaded = true; report() }))
  if (document.readyState === 'complete') ready()
  else addEventListener('load', ready, { once: true })
  new MutationObserver(() => {
    if (timer !== undefined) return
    timer = setTimeout(() => { timer = undefined; report() }, 100)
  }).observe(document.documentElement, { childList: true, subtree: true })
  addEventListener('message', (event) => {
    const data = event.data
    if (event.source !== parent || data?.type !== 'quality-test-svg-request' || data.previewID !== previewID || typeof data.requestID !== 'string' || data.requestID.length > 100) return
    try {
      if (!loaded) throw new Error('notReady')
      parent.postMessage({ type: 'quality-test-svg-result', previewID, requestID: data.requestID, svg: capture(limit) }, '*')
    } catch (error) {
      const code = error instanceof Error && ['noSVG', 'tooLarge', 'unsupported', 'notReady'].includes(error.message) ? error.message : 'failed'
      parent.postMessage({ type: 'quality-test-svg-result', previewID, requestID: data.requestID, error: code }, '*')
    }
  })
}

export function qualityTestSVGExportScript(previewID: string): string {
  const id = JSON.stringify(previewID).replace(/</g, '\\u003c')
  return `<script>(${installQualityTestSVGBridge.toString()})(${id},${captureQualityTestSVG.toString()},${QUALITY_TEST_SVG_LIMIT})</script>`
}

// Responses originate in a document containing arbitrary model code. Validate
// again in the parent, without inserting any returned nodes into the admin DOM.
export function validateQualityTestSVG(value: string): string {
  if (!value || new TextEncoder().encode(value).length > QUALITY_TEST_SVG_LIMIT) throw new Error('tooLarge')
  const parsed = new DOMParser().parseFromString(value, 'image/svg+xml')
  const root = parsed.documentElement
  if (parsed.querySelector('parsererror') || root.localName !== 'svg' || root.namespaceURI !== 'http://www.w3.org/2000/svg' || parsed.doctype) throw new Error('invalidSVG')
  for (const element of [root, ...root.querySelectorAll('*')]) {
    if (element.namespaceURI !== root.namespaceURI || ['script', 'style', 'foreignObject', 'animate', 'animateTransform', 'animateMotion', 'set', 'discard'].includes(element.localName)) throw new Error('invalidSVG')
    for (const attribute of [...element.attributes]) {
      if (/^on/i.test(attribute.localName) || attribute.localName === 'base') throw new Error('invalidSVG')
      if (attribute.localName === 'href' && !/^(?:#|data:image\/(?:png|jpeg|gif|webp|avif);base64,)/i.test(attribute.value)) throw new Error('invalidSVG')
      if (attribute.localName !== 'style' && /url\(/i.test(attribute.value) && !/^url\(["']?#[^)]*\)(?:\s+[^()]*)?$/.test(attribute.value)) throw new Error('invalidSVG')
    }
    const style = (element as SVGElement).style
    for (const property of style) {
      const css = style.getPropertyValue(property)
      // All paint servers must remain local to the exported SVG.
      if (/url\(/i.test(css) && !/^url\(["']?#[^)]*\)(?:\s+[^()]*)?$/.test(css)) throw new Error('invalidSVG')
      if (/^(?:animation|transition)/.test(property) || /image-set\(/i.test(css)) throw new Error('invalidSVG')
    }
  }
  return `<?xml version="1.0" encoding="UTF-8"?>\n${new XMLSerializer().serializeToString(root)}`
}

export function requestQualityTestSVG(frame: Window, previewID: string, signal: AbortSignal, host: Pick<Window, 'addEventListener' | 'removeEventListener'> = window, timeout = QUALITY_TEST_SVG_TIMEOUT): Promise<string> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new Error('cancelled')); return }
    const requestID = crypto.getRandomValues(new Uint32Array(4)).join('-')
    const cleanup = () => { clearTimeout(timer); host.removeEventListener('message', receive); signal.removeEventListener('abort', abort) }
    const fail = (code: string) => { cleanup(); reject(new Error(code)) }
    const abort = () => fail('cancelled')
    const receive = (event: MessageEvent) => {
      const data = event.data
      if (event.source !== frame || data?.type !== 'quality-test-svg-result' || data.previewID !== previewID || data.requestID !== requestID) return
      if (typeof data.error === 'string') { fail(data.error); return }
      if (typeof data.svg !== 'string' || !data.svg || data.svg.length > QUALITY_TEST_SVG_LIMIT) { fail('invalidSVG'); return }
      cleanup(); resolve(data.svg)
    }
    const timer = setTimeout(() => fail('timeout'), timeout)
    host.addEventListener('message', receive)
    signal.addEventListener('abort', abort, { once: true })
    try { frame.postMessage({ type: 'quality-test-svg-request', previewID, requestID }, '*') }
    catch { fail('failed') }
  })
}
