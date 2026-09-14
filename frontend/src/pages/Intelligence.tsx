import { useCallback, useEffect, useMemo, useState } from 'react'
import { Brain, RotateCw, Snowflake, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import { StatTile } from '../components/StatTile'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import type {
  CodexTurnStateCell,
  CodexTurnStateCellStatus,
  CodexTurnStateOverview,
  CodexTurnStateRefreshEvent,
} from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

/** 连续这么多轮刷不出健康值就算「长期降智」，和后端 chronicFailureThreshold 保持一致。 */
const CHRONIC_THRESHOLD = 3

const STATUS_TONE: Record<CodexTurnStateCellStatus, string> = {
  healthy: 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  stale: 'border-amber-500/30 bg-amber-500/10 text-amber-600 dark:text-amber-400',
  degraded: 'border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-400',
  unparsed: 'border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-400',
  cooling: 'border-orange-500/30 bg-orange-500/10 text-orange-600 dark:text-orange-400',
  missing: 'border-border bg-muted/50 text-muted-foreground',
}

/** 矩阵格子的实心配色，和上面的徽章同一套语义。 */
const MATRIX_TONE: Record<CodexTurnStateCellStatus, string> = {
  healthy: 'bg-emerald-500/80',
  stale: 'bg-amber-500/80',
  degraded: 'bg-red-500/80',
  unparsed: 'bg-red-500/80',
  cooling: 'bg-orange-500/80',
  missing: 'bg-muted-foreground/25',
}

type Row = CodexTurnStateCell & {
  accountId: number
  email: string
  planType: string
}

type PendingAction = `${number}:${string}`

export default function Intelligence() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [onlyProblems, setOnlyProblems] = useState(true)
  const [pending, setPending] = useState<PendingAction | null>(null)
  const [events, setEvents] = useState<CodexTurnStateRefreshEvent[]>([])

  const load = useCallback(() => api.getCodexTurnStateOverview(), [])
  const { data, loading, error, reload, reloadSilently } = useDataLoader<CodexTurnStateOverview | null>({
    initialData: null,
    load,
  })

  const loadEvents = useCallback(async () => {
    try {
      const response = await api.getCodexTurnStateRefreshEvents(200)
      setEvents(response.events ?? [])
    } catch {
      // 流水是辅助视图，拉取失败不该打断主表。
    }
  }, [])

  useEffect(() => {
    void loadEvents()
    const timer = window.setInterval(() => {
      void reloadSilently()
      void loadEvents()
    }, 20000)
    return () => window.clearInterval(timer)
  }, [reloadSilently, loadEvents])

  const allRows = useMemo<Row[]>(() => {
    const flat: Row[] = []
    for (const account of data?.accounts ?? []) {
      for (const cell of account.cells) {
        flat.push({ ...cell, accountId: account.account_id, email: account.email, planType: account.plan_type })
      }
    }
    // 先按连败次数，再按冷却剩余：最上面就是最该处理的无底洞。
    flat.sort((a, b) =>
      b.refresh_consecutive_fails - a.refresh_consecutive_fails ||
      b.cooldown_remaining_seconds - a.cooldown_remaining_seconds ||
      a.accountId - b.accountId ||
      a.model.localeCompare(b.model))
    return flat
  }, [data])

  const rows = useMemo(
    () => (onlyProblems ? allRows.filter((row) => row.status !== 'healthy') : allRows),
    [allRows, onlyProblems])

  // 矩阵的列 = 所有出现过的模型，按名字排序，保证每行列序一致。
  const models = useMemo(() => {
    const seen = new Set<string>()
    for (const account of data?.accounts ?? []) {
      for (const cell of account.cells) seen.add(cell.model)
    }
    return Array.from(seen).sort()
  }, [data])

  const runAction = useCallback(
    async (row: Row, action: 'refresh' | 'clearCooldown' | 'invalidate') => {
      const key: PendingAction = `${row.accountId}:${row.model}`
      setPending(key)
      try {
        const cell = { account_id: row.accountId, model: row.model }
        if (action === 'refresh') {
          const result = await api.refreshCodexTurnStateCell(cell)
          if (result.ok) {
            showToast(t('intelligence.action.refreshOk', { len: result.health?.cipher_len ?? 0 }), 'success')
          } else {
            showToast(t('intelligence.action.refreshFailed', { reason: result.error ?? '' }), 'error')
          }
        } else if (action === 'clearCooldown') {
          const result = await api.clearCodexTurnStateCooldown(cell)
          showToast(
            result.cleared ? t('intelligence.action.cooldownCleared') : t('intelligence.action.cooldownNone'),
            result.cleared ? 'success' : 'info')
        } else {
          await api.invalidateCodexTurnStateCell(cell)
          showToast(t('intelligence.action.invalidated'), 'success')
        }
        await reloadSilently()
        await loadEvents()
      } catch (err) {
        showToast(getErrorMessage(err), 'error')
      } finally {
        setPending(null)
      }
    },
    [loadEvents, reloadSilently, showToast, t])

  const summary = data?.summary

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('intelligence.loadingTitle')}
      errorTitle={t('intelligence.errorTitle')}
    >
      <>
        <PageHeader
          title={t('intelligence.title')}
          description={t('intelligence.desc')}
          onRefresh={() => {
            void reload()
            void loadEvents()
          }}
        />
        <OpsTabs />

        <div className="mb-5 grid grid-cols-2 gap-3 lg:grid-cols-5">
          <StatTile
            label={t('intelligence.stat.healthy')}
            value={`${summary?.healthy ?? 0} / ${summary?.cells ?? 0}`}
            icon={<Brain className="size-4" />}
            tone="success"
          />
          <StatTile label={t('intelligence.stat.degraded')} value={String(summary?.degraded ?? 0)} tone="danger" />
          <StatTile label={t('intelligence.stat.cooling')} value={String(summary?.cooling_down ?? 0)} tone="warning" />
          <StatTile label={t('intelligence.stat.missing')} value={String(summary?.missing ?? 0)} />
          <StatTile label={t('intelligence.stat.chronic')} value={String(summary?.chronic_failures ?? 0)} tone="danger" />
        </div>

        <Matrix accounts={data?.accounts ?? []} models={models} />

        <Card className="mt-5">
          <CardContent className="p-0">
            <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-4 py-3">
              <div>
                <h2 className="text-sm font-semibold text-foreground">{t('intelligence.table.title')}</h2>
                <p className="text-xs text-muted-foreground">{t('intelligence.table.hint')}</p>
              </div>
              <label className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground">
                <input
                  type="checkbox"
                  className="size-3.5"
                  checked={onlyProblems}
                  onChange={(e) => setOnlyProblems(e.target.checked)}
                />
                {t('intelligence.table.onlyProblems')}
              </label>
            </div>

            {rows.length === 0 ? (
              <p className="px-4 py-10 text-center text-sm text-muted-foreground">
                {onlyProblems ? t('intelligence.table.allHealthy') : t('intelligence.table.empty')}
              </p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-left text-[13px]">
                  <thead className="border-b border-border text-xs text-muted-foreground">
                    <tr>
                      <th className="px-4 py-2 font-medium">{t('intelligence.col.account')}</th>
                      <th className="px-4 py-2 font-medium">{t('intelligence.col.model')}</th>
                      <th className="px-4 py-2 font-medium">{t('intelligence.col.status')}</th>
                      <th className="px-4 py-2 font-medium">{t('intelligence.col.cipher')}</th>
                      <th className="px-4 py-2 text-right font-medium">{t('intelligence.col.fails')}</th>
                      <th className="px-4 py-2 text-right font-medium">{t('intelligence.col.cooldown')}</th>
                      <th className="px-4 py-2 text-right font-medium">{t('intelligence.col.lastRound')}</th>
                      <th className="px-4 py-2 font-medium">{t('intelligence.col.reason')}</th>
                      <th className="px-4 py-2 text-right font-medium">{t('intelligence.col.actions')}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row) => {
                      const busy = pending === `${row.accountId}:${row.model}`
                      return (
                        <tr key={`${row.accountId}-${row.model}`} className="border-b border-border/60 last:border-0">
                          <td className="px-4 py-2">
                            <span className="font-mono text-xs text-muted-foreground">#{row.accountId}</span>
                            <span className="ml-2">{row.email || '—'}</span>
                            {row.planType ? (
                              <span className="ml-2 text-xs text-muted-foreground">{row.planType}</span>
                            ) : null}
                          </td>
                          <td className="px-4 py-2 font-mono text-xs">{row.model}</td>
                          <td className="px-4 py-2">
                            <Badge variant="outline" className={STATUS_TONE[row.status]}>
                              {t(`intelligence.status.${row.status}`)}
                            </Badge>
                          </td>
                          <td className="px-4 py-2 font-mono text-xs">{cipherLabel(row, t)}</td>
                          <td className={`px-4 py-2 text-right font-mono text-xs ${row.refresh_consecutive_fails >= CHRONIC_THRESHOLD ? 'font-bold text-red-500' : ''}`}>
                            {row.refresh_consecutive_fails || '—'}
                          </td>
                          <td className="px-4 py-2 text-right font-mono text-xs">
                            {row.cooldown_remaining_seconds > 0 ? formatDuration(row.cooldown_remaining_seconds) : '—'}
                          </td>
                          <td className="px-4 py-2 text-right font-mono text-xs">
                            {row.refresh_last_ping_count > 0
                              ? t('intelligence.lastRound', {
                                  pings: row.refresh_last_ping_count,
                                  seconds: (row.refresh_last_duration_ms / 1000).toFixed(1),
                                })
                              : '—'}
                          </td>
                          <td className="px-4 py-2 text-xs text-muted-foreground">
                            {row.refresh_failure_kind
                              ? t(`intelligence.failure.${row.refresh_failure_kind}`, { defaultValue: row.refresh_failure_kind })
                              : '—'}
                          </td>
                          <td className="px-4 py-2">
                            <div className="flex items-center justify-end gap-1">
                              <Button
                                variant="ghost"
                                size="icon"
                                className="size-7"
                                disabled={busy}
                                title={t('intelligence.action.refresh')}
                                onClick={() => void runAction(row, 'refresh')}
                              >
                                <RotateCw className={`size-3.5 ${busy ? 'animate-spin' : ''}`} />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                className="size-7"
                                disabled={busy || row.cooldown_remaining_seconds <= 0}
                                title={t('intelligence.action.clearCooldown')}
                                onClick={() => void runAction(row, 'clearCooldown')}
                              >
                                <Snowflake className="size-3.5" />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                className="size-7"
                                disabled={busy || !row.has_value}
                                title={t('intelligence.action.invalidate')}
                                onClick={() => void runAction(row, 'invalidate')}
                              >
                                <Trash2 className="size-3.5" />
                              </Button>
                            </div>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </CardContent>
        </Card>

        <EventStream events={events} />

        <p className="mt-3 text-xs text-muted-foreground">
          {t('intelligence.footnote', { time: data?.generated_at ? new Date(data.generated_at).toLocaleTimeString() : '—' })}
        </p>
      </>
    </StateShell>
  )
}

function Matrix({ accounts, models }: { accounts: CodexTurnStateOverview['accounts']; models: string[] }) {
  const { t } = useTranslation()
  if (accounts.length === 0 || models.length === 0) return null

  return (
    <Card>
      <CardContent className="p-0">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-4 py-3">
          <div>
            <h2 className="text-sm font-semibold text-foreground">{t('intelligence.matrix.title')}</h2>
            <p className="text-xs text-muted-foreground">{t('intelligence.matrix.hint')}</p>
          </div>
          <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
            {(['healthy', 'stale', 'cooling', 'degraded', 'missing'] as CodexTurnStateCellStatus[]).map((status) => (
              <span key={status} className="inline-flex items-center gap-1.5">
                <span className={`inline-block size-2.5 rounded-sm ${MATRIX_TONE[status]}`} />
                {t(`intelligence.status.${status}`)}
              </span>
            ))}
          </div>
        </div>
        <div className="max-h-[420px] overflow-auto">
          <table className="w-full text-left text-xs">
            <thead className="sticky top-0 bg-card text-muted-foreground">
              <tr>
                <th className="px-4 py-2 font-medium">{t('intelligence.col.account')}</th>
                {models.map((model) => (
                  <th key={model} className="px-2 py-2 text-center font-mono font-medium">
                    {model.replace(/^gpt-/, '')}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {accounts.map((account) => {
                const byModel = new Map(account.cells.map((cell) => [cell.model, cell]))
                return (
                  <tr key={account.account_id} className="border-t border-border/60">
                    <td className="whitespace-nowrap px-4 py-1.5">
                      <span className="font-mono text-muted-foreground">#{account.account_id}</span>
                      <span className="ml-2">{account.email || '—'}</span>
                    </td>
                    {models.map((model) => {
                      const cell = byModel.get(model)
                      if (!cell) {
                        return <td key={model} className="px-2 py-1.5 text-center text-muted-foreground/40">·</td>
                      }
                      return (
                        <td key={model} className="px-2 py-1.5 text-center">
                          <span
                            className={`inline-block size-3.5 rounded-sm ${MATRIX_TONE[cell.status]}`}
                            title={`${model} · ${t(`intelligence.status.${cell.status}`)}${
                              cell.health && !cell.health.error
                                ? ` · ${cell.health.cipher_len}/${cell.health.expected_cipher_len}`
                                : ''
                            }${cell.refresh_consecutive_fails > 0 ? ` · ${cell.refresh_consecutive_fails}x` : ''}`}
                          />
                        </td>
                      )
                    })}
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  )
}

function EventStream({ events }: { events: CodexTurnStateRefreshEvent[] }) {
  const { t } = useTranslation()

  return (
    <Card className="mt-5">
      <CardContent className="p-0">
        <div className="border-b border-border px-4 py-3">
          <h2 className="text-sm font-semibold text-foreground">{t('intelligence.stream.title')}</h2>
          <p className="text-xs text-muted-foreground">{t('intelligence.stream.hint')}</p>
        </div>
        {events.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-muted-foreground">{t('intelligence.stream.empty')}</p>
        ) : (
          <div className="max-h-[320px] overflow-auto px-4 py-2 font-mono text-xs">
            {events.map((event) => (
              <div key={event.seq} className="flex gap-3 border-b border-border/40 py-1 last:border-0">
                <span className="shrink-0 text-muted-foreground">{new Date(event.at).toLocaleTimeString()}</span>
                <span className="shrink-0 text-muted-foreground">#{event.account_id}</span>
                <span className="shrink-0">{event.model}</span>
                <span className={`shrink-0 ${event.ok ? 'text-emerald-500' : 'text-red-500'}`}>
                  {event.ok ? 'OK' : t(`intelligence.failure.${event.failure_kind}`, { defaultValue: event.failure_kind ?? '' })}
                </span>
                <span className="shrink-0 text-muted-foreground">
                  {t('intelligence.lastRound', { pings: event.ping_count, seconds: (event.duration_ms / 1000).toFixed(1) })}
                </span>
                {event.cipher_len > 0 ? (
                  <span className="shrink-0 text-red-400">
                    {event.cipher_len}/{event.expected_cipher_len}
                  </span>
                ) : null}
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

/**
 * 密文长度列：优先展示缓存里那个值的实际/期望长度；没有缓存值时退回最近一次刷新
 * 拿到的降智长度。判定永远用 health / refresh_*，不用 base64 字符串长度做阈值——
 * 团队套餐的健康值是 332 字符，固定阈值会把它们全判成异常。
 */
function cipherLabel(row: Row, t: (key: string) => string): string {
  if (row.health && !row.health.error) {
    return `${row.health.cipher_len} / ${row.health.expected_cipher_len}`
  }
  if (row.health?.error) {
    return t('intelligence.cipher.unparsed')
  }
  if (row.refresh_degraded_cipher_len > 0) {
    return `${row.refresh_degraded_cipher_len} / ${row.refresh_expected_cipher_len}`
  }
  return '—'
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  return rest === 0 ? `${minutes}m` : `${minutes}m${rest}s`
}
