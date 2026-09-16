import { useCallback, useEffect, useMemo, useState } from 'react'
import { Brain, RotateCw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import { StatTile } from '../components/StatTile'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import type {
  CodexTurnStateAccountRow,
  CodexTurnStateCell,
  CodexTurnStateCellStatus,
  CodexTurnStateOverview,
  CodexTurnStateRefreshEvent,
} from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

/** 连续这么多轮刷不出健康值就算「长期降智」，和后端 chronicFailureThreshold 保持一致。 */
const CHRONIC_THRESHOLD = 3

const ALL_STATUSES: CodexTurnStateCellStatus[] = ['healthy', 'stale', 'cooling', 'degraded', 'unparsed', 'missing']

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

const PAGE_SIZE_OPTIONS = [20, 50, 100]

type Row = CodexTurnStateCell & {
  accountId: number
  email: string
  planType: string
}

type PendingAction = `${number}:${string}`

/** 状态筛选的取值：具体状态、全部、或「有问题的」（= 非健康）。 */
type StatusFilter = CodexTurnStateCellStatus | 'all' | 'problems'

interface Filters {
  search: string
  status: StatusFilter
  model: string
  plan: string
}

const DEFAULT_FILTERS: Filters = { search: '', status: 'problems', model: '', plan: '' }

function cellMatchesStatus(status: CodexTurnStateCellStatus, filter: StatusFilter): boolean {
  if (filter === 'all') return true
  if (filter === 'problems') return status !== 'healthy'
  return status === filter
}

function accountMatchesSearch(account: { account_id: number; email: string }, search: string): boolean {
  const needle = search.trim().toLowerCase()
  if (!needle) return true
  if (String(account.account_id) === needle || `#${account.account_id}` === needle) return true
  return account.email.toLowerCase().includes(needle)
}

export default function Intelligence() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [filters, setFilters] = useState<Filters>(DEFAULT_FILTERS)
  const [pending, setPending] = useState<PendingAction | null>(null)
  const [events, setEvents] = useState<CodexTurnStateRefreshEvent[]>([])
  const [matrixPage, setMatrixPage] = useState(1)
  const [matrixPageSize, setMatrixPageSize] = useState(20)
  const [listPage, setListPage] = useState(1)
  const [listPageSize, setListPageSize] = useState(20)

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

  const updateFilters = useCallback((patch: Partial<Filters>) => {
    setFilters((prev) => ({ ...prev, ...patch }))
    // 任何筛选变化都回到第一页，否则很容易停在一个超出范围的页码上。
    setMatrixPage(1)
    setListPage(1)
  }, [])

  // 下拉选项：所有出现过的模型和套餐，按名字排序。
  const { models, plans } = useMemo(() => {
    const modelSet = new Set<string>()
    const planSet = new Set<string>()
    for (const account of data?.accounts ?? []) {
      if (account.plan_type) planSet.add(account.plan_type)
      for (const cell of account.cells) modelSet.add(cell.model)
    }
    return { models: Array.from(modelSet).sort(), plans: Array.from(planSet).sort() }
  }, [data])

  // 矩阵：账号级筛选。搜索 / 套餐直接过滤账号；状态筛选保留「至少有一格命中」的账号，
  // 但该账号的整行都显示，否则看不出这一格在整体里的位置。模型筛选缩列。
  const matrixAccounts = useMemo<CodexTurnStateAccountRow[]>(() => {
    return (data?.accounts ?? []).filter((account) => {
      if (!accountMatchesSearch(account, filters.search)) return false
      if (filters.plan && account.plan_type !== filters.plan) return false
      const cells = filters.model ? account.cells.filter((c) => c.model === filters.model) : account.cells
      return cells.some((cell) => cellMatchesStatus(cell.status, filters.status))
    })
  }, [data, filters])
  const matrixModels = useMemo(
    () => (filters.model ? models.filter((m) => m === filters.model) : models),
    [models, filters.model])

  // 问题列表：格子级筛选，四个条件全部命中才留下。
  const listRows = useMemo<Row[]>(() => {
    const flat: Row[] = []
    for (const account of data?.accounts ?? []) {
      if (!accountMatchesSearch(account, filters.search)) continue
      if (filters.plan && account.plan_type !== filters.plan) continue
      for (const cell of account.cells) {
        if (filters.model && cell.model !== filters.model) continue
        if (!cellMatchesStatus(cell.status, filters.status)) continue
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
  }, [data, filters])

  const matrixTotalPages = Math.max(1, Math.ceil(matrixAccounts.length / matrixPageSize))
  const matrixSlice = useMemo(() => {
    const page = Math.min(matrixPage, matrixTotalPages)
    return matrixAccounts.slice((page - 1) * matrixPageSize, page * matrixPageSize)
  }, [matrixAccounts, matrixPage, matrixPageSize, matrixTotalPages])

  const listTotalPages = Math.max(1, Math.ceil(listRows.length / listPageSize))
  const listSlice = useMemo(() => {
    const page = Math.min(listPage, listTotalPages)
    return listRows.slice((page - 1) * listPageSize, page * listPageSize)
  }, [listRows, listPage, listPageSize, listTotalPages])

  const runAction = useCallback(
    async (row: Row, action: 'refresh' | 'invalidate') => {
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
  const statusOptions = useMemo(
    () => [
      { value: 'problems', label: t('intelligence.filter.statusProblems') },
      { value: 'all', label: t('intelligence.filter.statusAll') },
      ...ALL_STATUSES.map((status) => ({ value: status, label: t(`intelligence.status.${status}`) })),
    ],
    [t])

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

        <Card className="mb-5">
          <CardContent className="flex flex-wrap items-end gap-3 p-4">
            <label className="flex min-w-[220px] flex-1 flex-col gap-1 text-xs text-muted-foreground">
              {t('intelligence.filter.search')}
              <Input
                value={filters.search}
                placeholder={t('intelligence.filter.searchPlaceholder')}
                onChange={(e) => updateFilters({ search: e.target.value })}
              />
            </label>
            <label className="flex w-40 flex-col gap-1 text-xs text-muted-foreground">
              {t('intelligence.filter.status')}
              <Select
                value={filters.status}
                onValueChange={(value) => updateFilters({ status: value as StatusFilter })}
                options={statusOptions}
              />
            </label>
            <label className="flex w-44 flex-col gap-1 text-xs text-muted-foreground">
              {t('intelligence.filter.model')}
              <Select
                value={filters.model}
                onValueChange={(value) => updateFilters({ model: value })}
                options={[{ value: '', label: t('intelligence.filter.modelAll') }, ...models.map((m) => ({ value: m, label: m }))]}
              />
            </label>
            <label className="flex w-52 flex-col gap-1 text-xs text-muted-foreground">
              {t('intelligence.filter.plan')}
              <Select
                value={filters.plan}
                onValueChange={(value) => updateFilters({ plan: value })}
                options={[{ value: '', label: t('intelligence.filter.planAll') }, ...plans.map((p) => ({ value: p, label: p }))]}
              />
            </label>
            <Button variant="ghost" size="sm" onClick={() => updateFilters(DEFAULT_FILTERS)}>
              {t('intelligence.filter.reset')}
            </Button>
          </CardContent>
        </Card>

        <Matrix
          accounts={matrixSlice}
          models={matrixModels}
          totalAccounts={matrixAccounts.length}
          page={Math.min(matrixPage, matrixTotalPages)}
          totalPages={matrixTotalPages}
          pageSize={matrixPageSize}
          onPageChange={setMatrixPage}
          onPageSizeChange={(size) => {
            setMatrixPageSize(size)
            setMatrixPage(1)
          }}
        />

        <Card className="mt-5">
          <CardContent className="p-0">
            <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-4 py-3">
              <div>
                <h2 className="text-sm font-semibold text-foreground">{t('intelligence.table.title')}</h2>
                <p className="text-xs text-muted-foreground">{t('intelligence.table.hint')}</p>
              </div>
              <span className="text-xs text-muted-foreground">
                {t('intelligence.table.count', { count: listRows.length })}
              </span>
            </div>

            {listRows.length === 0 ? (
              <p className="px-4 py-10 text-center text-sm text-muted-foreground">
                {filters.status === 'problems' && !filters.search && !filters.model && !filters.plan
                  ? t('intelligence.table.allHealthy')
                  : t('intelligence.table.noMatch')}
              </p>
            ) : (
              <>
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
                      {listSlice.map((row) => {
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
                            <td className="px-4 py-2 font-mono text-xs">
                              <CipherCell row={row} />
                            </td>
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
                <div className="border-t border-border px-4 py-3">
                  <Pagination
                    page={Math.min(listPage, listTotalPages)}
                    totalPages={listTotalPages}
                    onPageChange={setListPage}
                    totalItems={listRows.length}
                    pageSize={listPageSize}
                    pageSizeOptions={PAGE_SIZE_OPTIONS}
                    onPageSizeChange={(size) => {
                      setListPageSize(size)
                      setListPage(1)
                    }}
                  />
                </div>
              </>
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

function Matrix({
  accounts,
  models,
  totalAccounts,
  page,
  totalPages,
  pageSize,
  onPageChange,
  onPageSizeChange,
}: {
  accounts: CodexTurnStateAccountRow[]
  models: string[]
  totalAccounts: number
  page: number
  totalPages: number
  pageSize: number
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
}) {
  const { t } = useTranslation()

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
        {accounts.length === 0 || models.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-muted-foreground">{t('intelligence.table.noMatch')}</p>
        ) : (
          <>
            <div className="overflow-auto">
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
                          {account.plan_type ? (
                            <span className="ml-2 text-muted-foreground">{account.plan_type}</span>
                          ) : null}
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
            <div className="border-t border-border px-4 py-3">
              <Pagination
                page={page}
                totalPages={totalPages}
                onPageChange={onPageChange}
                totalItems={totalAccounts}
                pageSize={pageSize}
                pageSizeOptions={PAGE_SIZE_OPTIONS}
                onPageSizeChange={onPageSizeChange}
              />
            </div>
          </>
        )}
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
 * 密文长度列。两份数据可能同时存在且不一致：缓存里的旧 blob（可能健康但已过期）
 * 和最近一轮刷新拿到的 blob（可能降智）。「校验未通过」说的是后者，所以最近一轮
 * 拿到降智值时以它为主，缓存值降为副行；否则显示缓存值。
 * 判定永远用 health / refresh_*，不用 base64 字符串长度做阈值——团队套餐的健康值是
 * 332 字符，固定阈值会把它们全判成异常。
 */
function CipherCell({ row }: { row: Row }) {
  const { t } = useTranslation()
  const cached = row.health && !row.health.error ? row.health : null
  const latestDegraded = row.refresh_failure_kind === 'degraded' && row.refresh_degraded_cipher_len > 0

  if (latestDegraded) {
    return (
      <div className="leading-tight">
        <div className="text-red-500">
          {row.refresh_degraded_cipher_len} / {row.refresh_expected_cipher_len}
        </div>
        {cached ? (
          <div className="text-[10px] text-muted-foreground">
            {t('intelligence.cipher.cachedValue', { len: cached.cipher_len })}
          </div>
        ) : null}
      </div>
    )
  }
  if (cached) {
    return <>{cached.cipher_len} / {cached.expected_cipher_len}</>
  }
  if (row.health?.error) {
    return <>{t('intelligence.cipher.unparsed')}</>
  }
  return <>—</>
}

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  return rest === 0 ? `${minutes}m` : `${minutes}m${rest}s`
}
