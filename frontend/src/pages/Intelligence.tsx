import { useCallback, useEffect, useMemo, useState } from 'react'
import { Brain } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import { StatTile } from '../components/StatTile'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import type { CodexTurnStateCell, CodexTurnStateCellStatus, CodexTurnStateOverview } from '../types'
import { Badge } from '@/components/ui/badge'
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

type ProblemRow = CodexTurnStateCell & {
  accountId: number
  email: string
  planType: string
}

export default function Intelligence() {
  const { t } = useTranslation()
  const [onlyProblems, setOnlyProblems] = useState(true)
  const load = useCallback(() => api.getCodexTurnStateOverview(), [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<CodexTurnStateOverview | null>({
    initialData: null,
    load,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 20000)
    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const rows = useMemo<ProblemRow[]>(() => {
    const flat: ProblemRow[] = []
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
    return onlyProblems ? flat.filter((row) => row.status !== 'healthy') : flat
  }, [data, onlyProblems])

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
          onRefresh={() => void reload()}
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

        <Card>
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
                    </tr>
                  </thead>
                  <tbody>
                    {rows.map((row) => (
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
                          {row.refresh_failure_kind ? t(`intelligence.failure.${row.refresh_failure_kind}`, { defaultValue: row.refresh_failure_kind }) : '—'}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </CardContent>
        </Card>

        <p className="mt-3 text-xs text-muted-foreground">
          {t('intelligence.footnote', { time: data?.generated_at ? new Date(data.generated_at).toLocaleTimeString() : '—' })}
        </p>
      </>
    </StateShell>
  )
}

/**
 * 密文长度列：优先展示缓存里那个值的实际/期望长度；没有缓存值时退回最近一次刷新
 * 拿到的降智长度。判定永远用 health / refresh_*，不用 base64 字符串长度做阈值——
 * 团队套餐的健康值是 332 字符，固定阈值会把它们全判成异常。
 */
function cipherLabel(row: ProblemRow, t: (key: string) => string): string {
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
