export const FREE_POOL_PAGE_SIZE = 50

export const FREE_POOL_ACCOUNT_STATUSES = ['active', 'disabled'] as const
export type FreePoolAccountStatus = (typeof FREE_POOL_ACCOUNT_STATUSES)[number]

export const FREE_POOL_TICKET_STATUSES = ['ready', 'leased'] as const
export type FreePoolTicketStatus = (typeof FREE_POOL_TICKET_STATUSES)[number]

export interface FreePoolPage<T> {
  items: T[]
  next_before_id: number | null
}

export interface FreePoolAccountSummary {
  id: number
  name: string
  status: FreePoolAccountStatus
  proxy_url: string
  cooldown_until: number | null
  has_error: boolean
  created_at: number
  updated_at: number
}

export interface FreePoolTicketSummary {
  id: number
  source_account_id: number
  mint_model: string
  source_gateway: string
  source_colo: string
  issued_at: number
  hard_expires_at: number
  status: FreePoolTicketStatus
  hard_expired: boolean
  has_quarantine_reason: boolean
}

export interface FreePoolProbeAggregate {
  stage: 'qualification' | 'followup'
  status: 'pass' | 'degraded' | 'failed' | 'unknown'
  error_code: '' | 'timeout' | 'disconnected' | 'incomplete' | 'upstream_error' | 'invalid_response'
  count: number
  new_state_count: number
  last_created_at: number
}

export interface FreePoolProbeSummary {
  ticket_id: number
  total: number
  items: FreePoolProbeAggregate[]
}

export interface CreateFreePoolAccountRequest {
  name: string
  status: FreePoolAccountStatus
  proxy_url: string
  credentials: Record<string, unknown>
}

export interface FreePoolAccountListParams {
  limit?: number
  before_id?: number
  status?: FreePoolAccountStatus | ''
}

export interface FreePoolTicketListParams {
  limit?: number
  before_id?: number
  status?: FreePoolTicketStatus | ''
  source_account_id?: number
}

export interface FreePoolAccountDraft {
  name: string
  status: FreePoolAccountStatus
  proxy_url: string
  credentials: string
}

export function newFreePoolAccountDraft(): FreePoolAccountDraft {
  return { name: '', status: 'disabled', proxy_url: '', credentials: '' }
}

export function freePoolQuery(params: FreePoolAccountListParams | FreePoolTicketListParams = {}): string {
  const query = new URLSearchParams()
  query.set('limit', String(params.limit ?? FREE_POOL_PAGE_SIZE))
  if (params.before_id) query.set('before_id', String(params.before_id))
  if (params.status) query.set('status', params.status)
  if ('source_account_id' in params && params.source_account_id) {
    query.set('source_account_id', String(params.source_account_id))
  }
  return query.toString()
}

function byteLength(value: string): number {
  return new TextEncoder().encode(value).byteLength
}

type AccountValidation =
  | { ok: true; value: CreateFreePoolAccountRequest }
  | { ok: false; errorKey: string }

export function validateFreePoolAccount(draft: FreePoolAccountDraft): AccountValidation {
  const name = draft.name.trim()
  const proxyURL = draft.proxy_url.trim()
  if (!name || byteLength(name) > 128) return { ok: false, errorKey: 'freePool.validation.name' }
  if (draft.status !== 'active' && draft.status !== 'disabled') return { ok: false, errorKey: 'freePool.validation.status' }
  if (byteLength(proxyURL) > 4096) return { ok: false, errorKey: 'freePool.validation.proxy' }
  if (proxyURL) {
    try {
      const parsed = new URL(proxyURL)
      if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(parsed.protocol) || !parsed.hostname) {
        return { ok: false, errorKey: 'freePool.validation.proxy' }
      }
    } catch {
      return { ok: false, errorKey: 'freePool.validation.proxy' }
    }
  }
  if (byteLength(draft.credentials) > 64 * 1024) return { ok: false, errorKey: 'freePool.validation.credentialsSize' }
  let credentials: unknown
  try {
    credentials = JSON.parse(draft.credentials)
  } catch {
    return { ok: false, errorKey: 'freePool.validation.credentials' }
  }
  if (typeof credentials !== 'object' || credentials === null || Array.isArray(credentials) || Object.keys(credentials).length === 0) {
    return { ok: false, errorKey: 'freePool.validation.credentials' }
  }
  if (byteLength(JSON.stringify(credentials)) > 64 * 1024) return { ok: false, errorKey: 'freePool.validation.credentialsSize' }
  return { ok: true, value: { name, status: draft.status, proxy_url: proxyURL, credentials: credentials as Record<string, unknown> } }
}

export function parseFreePoolSourceID(text: string): { ok: true; value: number } | { ok: false; errorKey: string } {
  const value = text.trim()
  if (!value) return { ok: true, value: 0 }
  if (!/^[1-9]\d*$/.test(value)) return { ok: false, errorKey: 'freePool.validation.sourceID' }
  const id = Number(value)
  if (!Number.isSafeInteger(id)) return { ok: false, errorKey: 'freePool.validation.sourceID' }
  return { ok: true, value: id }
}

export function freePoolErrorKey(status?: number): string {
  switch (status) {
    case 400: return 'freePool.errors.invalidRequest'
    case 401:
    case 403: return 'freePool.errors.authRequired'
    case 404: return 'freePool.errors.notFound'
    case 501: return 'freePool.errors.unsupported'
    default: return 'freePool.errors.requestFailed'
  }
}
