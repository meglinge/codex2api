import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowDown, ArrowUp, ArrowUpDown, BarChart3, ChevronDown, ChevronRight } from 'lucide-react'
import { Bar, BarChart, CartesianGrid, Cell, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

import { api } from '../api'
import { cloudflareColos } from '../lib/cloudflareColos'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

type Gateway = { gateway: string; edge_colo: string; edges: string[]; total: number; passed: number; failed: number; pending: number; unused_ready: number }
type NodeRow = Omit<Gateway, 'edges'> & { edges: Gateway[] }
type Stats = { total: number; passed: number; failed: number; pending: number; nodes: number; gateways: Gateway[] }
type SortKey = 'gateway' | 'passed' | 'failed' | 'pending' | 'rate' | 'total' | 'unused'
type Outcome = 'all' | 'passed' | 'failed' | 'pending'

const pieColors = ['hsl(142 71% 45%)', 'hsl(0 72% 51%)', 'hsl(36 90% 55%)']

function percent(passed: number, decided: number) {
  if (decided <= 0) return '—'
  return `${Math.round((passed / decided) * 1000) / 10}%`
}

function rateValue(item: { passed: number; failed: number }) {
  const decided = item.passed + item.failed
  return decided <= 0 ? -1 : item.passed / decided
}

function gatewayNumber(gateway: string) {
  const match = /(\d+)\s*$/.exec(gateway)
  return match ? Number(match[1]) : Number.POSITIVE_INFINITY
}

function groupNodes(items: Gateway[]) {
  const groups = new Map<string, NodeRow>()
  for (const item of items) {
    const key = item.gateway || ''
    const current = groups.get(key) ?? { ...item, edge_colo: '', edges: [], total: 0, passed: 0, failed: 0, pending: 0, unused_ready: 0 }
    current.total += item.total
    current.passed += item.passed
    current.failed += item.failed
    current.pending += item.pending
    current.unused_ready = Math.max(current.unused_ready, item.unused_ready)
    if (item.edge_colo || item.total > 0) current.edges.push(item)
    groups.set(key, current)
  }
  return [...groups.values()]
}

export default function FreePoolFirstUse() {
  const { t } = useTranslation()
  const [hours, setHours] = useState(24)
  const [query, setQuery] = useState('')
  const [outcome, setOutcome] = useState<Outcome>('all')
  const [sortKey, setSortKey] = useState<SortKey>('gateway')
  const [sortDesc, setSortDesc] = useState(false)
  const [open, setOpen] = useState('')
  const [revision, setRevision] = useState(0)
  const [state, setState] = useState<{ kind: 'loading' } | { kind: 'ready'; data: Stats } | { kind: 'error' }>({ kind: 'loading' })
  const load = useCallback((signal: AbortSignal) => api.getFreePoolFirstUseStats(hours, signal), [hours])

  useEffect(() => {
    const controller = new AbortController()
    setState({ kind: 'loading' })
    void load(controller.signal).then(
      (data) => { if (!controller.signal.aborted) setState({ kind: 'ready', data }) },
      () => { if (!controller.signal.aborted) setState({ kind: 'error' }) },
    )
    return () => controller.abort()
  }, [load, revision])

  const rows = useMemo(() => {
    if (state.kind !== 'ready') return []
    const needle = query.trim().toLowerCase()
    return groupNodes(state.data.gateways.filter((item) => {
      const places = [item.edge_colo, ...item.edges].map((code) => edgeLabel(code)).join(' ')
      return !needle || `${item.gateway} ${places}`.toLowerCase().includes(needle)
    }))
      .filter((item) => outcome === 'all' || item[outcome] > 0 || item.edges.some((edge) => edge[outcome] > 0))
      .sort((a, b) => {
        const value = (item: NodeRow) => sortKey === 'gateway' ? gatewayNumber(item.gateway) : sortKey === 'rate' ? rateValue(item) : sortKey === 'unused' ? item.unused_ready : item[sortKey]
        const left = value(a)
        const right = value(b)
        const delta = Number(left) - Number(right) || a.gateway.localeCompare(b.gateway)
        return sortDesc ? -delta : delta
      })
  }, [outcome, query, sortDesc, sortKey, state, t])

  const chartRows = rows.filter((item) => item.passed + item.failed > 0).slice(0, 16)
  const decided = state.kind === 'ready' ? state.data.passed + state.data.failed : 0
  const pie = state.kind === 'ready' ? [
    { key: 'passed', name: t('freePool.firstUsePassed'), value: state.data.passed, fill: pieColors[0] },
    { key: 'failed', name: t('freePool.firstUseFailed'), value: state.data.failed, fill: pieColors[1] },
    { key: 'pending', name: t('freePool.firstUsePending'), value: state.data.pending, fill: pieColors[2] },
  ].filter((item) => item.value > 0) : []

  const sortBy = (key: SortKey) => {
    if (sortKey === key) setSortDesc((current) => !current)
    else {
      setSortKey(key)
      setSortDesc(key !== 'gateway')
    }
  }

  return (
    <div className="space-y-4">
      <PageHeader title={t('freePool.firstUseTitle')} description={t('freePool.firstUseHint')} />
      <div className="flex flex-wrap items-center gap-2">
        {[1, 6, 24, 72].map((item) => (
          <Button key={item} type="button" variant={hours === item ? 'default' : 'outline'} onClick={() => setHours(item)}>{t('freePool.firstUseHours', { hours: item })}</Button>
        ))}
        <Button type="button" variant="outline" onClick={() => setRevision((n) => n + 1)}>{t('freePool.refresh')}</Button>
      </div>
      {state.kind !== 'ready' ? <StateShell loading={state.kind === 'loading'} error={state.kind === 'error' ? t('freePool.errors.requestFailed') : undefined}><span /></StateShell> : (
        <>
          <div className="grid gap-3 sm:grid-cols-5">
            <Card><CardContent className="p-4"><div className="text-sm text-muted-foreground">{t('freePool.firstUseNodes')}</div><div className="text-2xl font-semibold">{state.data.nodes}</div></CardContent></Card>
            <Card><CardContent className="p-4"><div className="text-sm text-muted-foreground">{t('freePool.firstUseRate')}</div><div className="text-2xl font-semibold">{percent(state.data.passed, decided)}</div></CardContent></Card>
            <Card><CardContent className="p-4"><div className="text-sm text-muted-foreground">{t('freePool.firstUsePassed')}</div><div className="text-2xl font-semibold">{state.data.passed}</div></CardContent></Card>
            <Card><CardContent className="p-4"><div className="text-sm text-muted-foreground">{t('freePool.firstUseFailed')}</div><div className="text-2xl font-semibold">{state.data.failed}</div></CardContent></Card>
            <Card><CardContent className="p-4"><div className="text-sm text-muted-foreground">{t('freePool.firstUsePending')}</div><div className="text-2xl font-semibold">{state.data.pending}</div></CardContent></Card>
          </div>
          <div className="grid gap-3 lg:grid-cols-2">
            <Card>
              <CardContent className="p-4">
                <div className="mb-2 flex items-center gap-2 text-sm font-medium"><BarChart3 className="size-4" />{t('freePool.firstUseChart')}</div>
                {chartRows.length === 0 ? <p className="py-16 text-center text-sm text-muted-foreground">{t('freePool.empty')}</p> : (
                  <div className="h-72">
                    <ResponsiveContainer width="100%" height="100%">
                      <BarChart data={chartRows.map((item) => ({ ...item, name: item.gateway || t('freePool.firstUseUnknown'), rate: Math.round(rateValue(item) * 1000) / 10 }))} margin={{ top: 8, right: 8, left: 0, bottom: 48 }}>
                        <CartesianGrid vertical={false} stroke="var(--color-border)" strokeDasharray="4 4" />
                        <XAxis dataKey="name" interval={0} angle={-35} textAnchor="end" height={70} tick={{ fill: 'var(--color-muted-foreground)', fontSize: 11 }} />
                        <YAxis allowDecimals={false} tick={{ fill: 'var(--color-muted-foreground)', fontSize: 12 }} width={36} />
                        <Tooltip contentStyle={{ background: 'var(--color-card)', border: '1px solid var(--color-border)', borderRadius: 12, fontSize: 12 }} />
                        <Bar dataKey="passed" name={t('freePool.firstUsePassed')} stackId="result" fill="hsl(142 71% 45%)" />
                        <Bar dataKey="failed" name={t('freePool.firstUseFailed')} stackId="result" fill="hsl(0 72% 51%)" radius={[4, 4, 0, 0]} />
                      </BarChart>
                    </ResponsiveContainer>
                  </div>
                )}
              </CardContent>
            </Card>
            <Card>
              <CardContent className="p-4">
                <div className="mb-2 text-sm font-medium">{t('freePool.firstUseShare')}</div>
                {pie.length === 0 ? <p className="py-16 text-center text-sm text-muted-foreground">{t('freePool.empty')}</p> : (
                  <div className="h-72">
                    <ResponsiveContainer width="100%" height="100%">
                      <PieChart>
                        <Pie data={pie} dataKey="value" nameKey="name" innerRadius="62%" outerRadius="84%" strokeWidth={0}>
                          {pie.map((item) => <Cell key={item.key} fill={item.fill} />)}
                        </Pie>
                        <Tooltip contentStyle={{ background: 'var(--color-card)', border: '1px solid var(--color-border)', borderRadius: 12, fontSize: 12 }} />
                      </PieChart>
                    </ResponsiveContainer>
                  </div>
                )}
              </CardContent>
            </Card>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Input className="w-full sm:w-64" value={query} placeholder={t('freePool.firstUseSearch')} onChange={(event) => setQuery(event.target.value)} />
            {(['all', 'passed', 'failed', 'pending'] as const).map((item) => (
              <Button key={item} type="button" variant={outcome === item ? 'default' : 'outline'} onClick={() => setOutcome(item)}>{t(`freePool.firstUseFilter.${item}`)}</Button>
            ))}
          </div>
          <Card>
            <CardContent className="p-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <SortHead label={t('freePool.firstUseGateway')} active={sortKey === 'gateway'} desc={sortDesc} onClick={() => sortBy('gateway')} />
                    <SortHead label={t('freePool.firstUseUnused')} active={sortKey === 'unused'} desc={sortDesc} onClick={() => sortBy('unused')} />
                    <TableHead>{t('freePool.firstUseEdges')}</TableHead>
                    <SortHead label={t('freePool.firstUsePassed')} active={sortKey === 'passed'} desc={sortDesc} onClick={() => sortBy('passed')} />
                    <SortHead label={t('freePool.firstUseFailed')} active={sortKey === 'failed'} desc={sortDesc} onClick={() => sortBy('failed')} />
                    <SortHead label={t('freePool.firstUsePending')} active={sortKey === 'pending'} desc={sortDesc} onClick={() => sortBy('pending')} />
                    <SortHead label={t('freePool.firstUseTotal')} active={sortKey === 'total'} desc={sortDesc} onClick={() => sortBy('total')} />
                    <SortHead label={t('freePool.firstUseRate')} active={sortKey === 'rate'} desc={sortDesc} onClick={() => sortBy('rate')} />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.length === 0 ? (
                    <TableRow><TableCell colSpan={8} className="py-8 text-center text-muted-foreground">{t('freePool.empty')}</TableCell></TableRow>
                  ) : rows.map((item) => {
                    const known = item.edges.filter((edge) => edge.edge_colo)
                    const expanded = open === item.gateway
                    return (
                      <Fragment key={item.gateway || 'unknown'}>
                        <TableRow key={item.gateway || 'unknown'} className={known.length ? 'cursor-pointer' : undefined} onClick={() => known.length && setOpen(expanded ? '' : item.gateway)}>
                          <TableCell>
                            <span className="inline-flex items-center gap-1">
                              {known.length ? (expanded ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />) : <span className="inline-block size-3.5" />}
                              {item.gateway || t('freePool.firstUseUnknown')}
                            </span>
                          </TableCell>
                          <TableCell>{item.unused_ready}</TableCell>
                          <TableCell>{known.length || t('freePool.firstUseEdgeUnknown')}</TableCell>
                          <TableCell>{item.passed}</TableCell>
                          <TableCell>{item.failed}</TableCell>
                          <TableCell>{item.pending}</TableCell>
                          <TableCell>{item.total}</TableCell>
                          <TableCell>{percent(item.passed, item.passed + item.failed)}</TableCell>
                        </TableRow>
                        {expanded && known
                          .slice()
                          .sort((a, b) => (b.passed + b.failed) - (a.passed + a.failed) || a.edge_colo.localeCompare(b.edge_colo))
                          .map((edge) => (
                            <TableRow key={`${item.gateway}|${edge.edge_colo}`} className="bg-muted/30">
                              <TableCell className="pl-8 text-muted-foreground">{edgeLabel(edge.edge_colo)}</TableCell>
                              <TableCell />
                              <TableCell>{edge.edge_colo}</TableCell>
                              <TableCell>{edge.passed}</TableCell>
                              <TableCell>{edge.failed}</TableCell>
                              <TableCell>{edge.pending}</TableCell>
                              <TableCell>{edge.total}</TableCell>
                              <TableCell>{percent(edge.passed, edge.passed + edge.failed)}</TableCell>
                            </TableRow>
                          ))}
                      </Fragment>
                    )
                  })}
                </TableBody>
              </Table>
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}

function edgeLabel(code: string) {
  const place = cloudflareColos[code]
  if (!place) return code
  return `${place.city} · ${place.country} · ${code}`
}

function SortHead({ label, active, desc, onClick }: { label: string; active: boolean; desc: boolean; onClick: () => void }) {
  const Icon = !active ? ArrowUpDown : desc ? ArrowDown : ArrowUp
  return (
    <TableHead>
      <button type="button" className="inline-flex items-center gap-1" onClick={onClick}>
        {label}<Icon className="size-3.5" />
      </button>
    </TableHead>
  )
}
