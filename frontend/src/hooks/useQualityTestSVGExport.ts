import { useCallback, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { qualityTestPreviewDocument } from '../lib/qualityTest'
import { qualityTestSVGExportScript, requestQualityTestSVG, validateQualityTestSVG } from '../lib/qualityTestSVG'

export function useQualityTestSVGExport(html: string, generation: string, enabled: boolean) {
  const frameRef = useRef<HTMLIFrameElement>(null)
  const pending = useRef<AbortController | null>(null)
  // getRandomValues also works for installations served over plain HTTP.
  const previewID = useMemo(() => crypto.getRandomValues(new Uint32Array(4)).join('-'), [html, generation, enabled])
  const preview = useMemo(() => html && enabled ? qualityTestPreviewDocument(html, qualityTestSVGExportScript(previewID)) : '', [html, enabled, previewID])
  const [status, setStatus] = useState<{ previewID: string; hasSVG: boolean }>()
  const [exporting, setExporting] = useState(false)
  const ready = enabled && status?.previewID === previewID
  const hasSVG = ready && status.hasSVG

  // Cancel during commit, before a queued reply from the old frame can download
  // after a new record or source view has already appeared on screen.
  useLayoutEffect(() => {
    setExporting(false)
    const receive = (event: MessageEvent) => {
      if (!enabled || event.source !== frameRef.current?.contentWindow || event.data?.type !== 'quality-test-svg-ready' || event.data.previewID !== previewID || typeof event.data.hasSVG !== 'boolean') return
      setStatus({ previewID, hasSVG: event.data.hasSVG })
    }
    window.addEventListener('message', receive)
    return () => {
      window.removeEventListener('message', receive)
      pending.current?.abort()
      pending.current = null
    }
  }, [previewID, enabled])

  const exportSVG = useCallback(async () => {
    const frame = frameRef.current?.contentWindow
    if (!frame || !ready) throw new Error('notReady')
    if (pending.current) throw new Error('cancelled')
    const controller = new AbortController()
    pending.current = controller
    setExporting(true)
    try {
      const svg = await requestQualityTestSVG(frame, previewID, controller.signal)
      if (controller.signal.aborted) throw new Error('cancelled')
      return validateQualityTestSVG(svg)
    } finally {
      if (pending.current === controller) {
        pending.current = null
        setExporting(false)
      }
    }
  }, [previewID, ready])

  return { frameRef, preview, ready, hasSVG, exporting, exportSVG }
}

export type QualityTestSVGExport = ReturnType<typeof useQualityTestSVGExport>
