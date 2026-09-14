// 边缘拦截（Cloudflare WAF、反向代理错误页）识别。
//
// 这类响应根本没到源站：状态码通常是 403/503，Content-Type 是 text/html，
// 响应体是一整张拦截页。前端若直接 res.json() 会抛出
// “JSON.parse: unexpected character”，把真正的原因（WAF 命中、Ray ID）藏起来。
// 这里把它识别出来，让报错里带上厂商、状态码和 Ray ID，一眼能定位到去哪查。

export type EdgeBlockVendor = 'cloudflare' | 'unknown'

export type EdgeBlockInfo = {
  status: number
  vendor: EdgeBlockVendor
  /** Cloudflare 的 cf-ray（响应头优先，取不到则从拦截页正文里抠），拿不到为空串。 */
  rayId: string
}

type HeadersLike = { get(name: string): string | null | undefined }
type ResponseLike = { status: number; headers?: HeadersLike | null }

type Translate = (key: string, vars?: Record<string, string | number>) => string

// 拦截页里的形态是 `Cloudflare Ray ID: <strong>a3af1292ac7c65fb</strong>`，
// 数字后面可能再跟机房代码（-MNL）。中间的标签数量不固定，所以跳过任意标签。
const RAY_ID_IN_BODY = /Ray ID:?\s*(?:<[^>]*>\s*)*([0-9a-f]{16}(?:-[A-Za-z]{2,4})?)/i

function header(res: ResponseLike, name: string): string {
  return (res.headers?.get(name) ?? '').trim()
}

/**
 * detectEdgeBlock 判断这条响应是不是边缘拦截页；是则给出厂商与 Ray ID，否则返回 null。
 * 判据是“响应体是 HTML 而不是接口 JSON”，调用方只在解析失败或请求失败时问它。
 */
export function detectEdgeBlock(res: ResponseLike, body: string): EdgeBlockInfo | null {
  const contentType = header(res, 'content-type').toLowerCase()
  const isHTML =
    contentType.includes('text/html') || /^\s*<(?:!doctype|html|head|body)\b/i.test(body)
  if (!isHTML) return null

  const rayHeader = header(res, 'cf-ray')
  const isCloudflare =
    rayHeader !== '' ||
    header(res, 'server').toLowerCase().includes('cloudflare') ||
    /cloudflare/i.test(body)

  return {
    status: res.status,
    vendor: isCloudflare ? 'cloudflare' : 'unknown',
    rayId: rayHeader || (RAY_ID_IN_BODY.exec(body)?.[1] ?? ''),
  }
}

/** edgeBlockMessage 把拦截信息拼成给用户看的一句话；translate 由调用方注入（i18n.t 或组件里的 t）。 */
export function edgeBlockMessage(info: EdgeBlockInfo, translate: Translate): string {
  const vendor = info.vendor === 'cloudflare' ? 'Cloudflare' : translate('common.edgeBlockVendor')
  return info.rayId
    ? translate('common.edgeBlockedWithRay', {
        vendor,
        status: info.status,
        rayId: info.rayId,
      })
    : translate('common.edgeBlocked', { vendor, status: info.status })
}

/**
 * parseJSONResponse 读响应体并解析 JSON；不是 JSON 时优先报“被谁拦了”，
 * 而不是把 JSON.parse 的字符位置抛给用户。空体返回 undefined。
 */
export async function parseJSONResponse<T>(
  res: Response,
  translate: Translate,
): Promise<T | undefined> {
  const body = await res.text()
  if (!body.trim()) return undefined
  try {
    return JSON.parse(body) as T
  } catch (err) {
    const blocked = detectEdgeBlock(res, body)
    if (blocked) throw new Error(edgeBlockMessage(blocked, translate))
    throw err
  }
}
