import { type FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { AdminAPIError, api } from '../api'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import {
  FREE_POOL_ACCOUNT_STATUSES,
  FREE_POOL_TICKET_STATUSES,
  freePoolErrorKey,
  newFreePoolAccountDraft,
  parseFreePoolSourceID,
  validateFreePoolAccount,
  type FreePoolAccountStatus,
  type FreePoolAccountSummary,
  type FreePoolTicketStatus,
} from '../lib/freePool'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

type ResourceState<T> = { kind: 'loading' } | { kind: 'ready'; data: T } | { kind: 'error'; errorKey: string }

function errorKey(error: unknown): string {
  return freePoolErrorKey(error instanceof AdminAPIError ? error.status : undefined)
}

function useResource<T>(load: (signal: AbortSignal) => Promise<T>) {
  const [revision, setRevision] = useState(0)
  const [state, setState] = useState<ResourceState<T>>({ kind: 'loading' })

  useEffect(() => {
    const controller = new AbortController()
    setState({ kind: 'loading' })
    void load(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) setState({ kind: 'ready', data })
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setState({ kind: 'error', errorKey: errorKey(error) })
      },
    )
    return () => controller.abort()
  }, [load, revision])

  return { state, reload: useCallback(() => setRevision((value) => value + 1), []) }
}

function formatTime(value: number | null, language: string): string {
  return value === null ? '—' : new Date(value).toLocaleString(language)
}

function CursorPager({ history, next, count, disabled = false, onChange }: {
  history: number[]
  next: number | null
  count: number
  disabled?: boolean
  onChange: (history: number[]) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <p className="text-sm text-muted-foreground" aria-live="polite">{t('freePool.page', { page: history.length, total: count })}</p>
      <div className="flex flex-wrap gap-2">
        <Button type="button" variant="outline" size="sm" disabled={disabled || history.length === 1} onClick={() => onChange([0])}>{t('freePool.first')}</Button>
        <Button type="button" variant="outline" size="sm" disabled={disabled || history.length === 1} onClick={() => onChange(history.slice(0, -1))}>{t('freePool.previous')}</Button>
        <Button type="button" variant="outline" size="sm" disabled={disabled || next === null} onClick={() => { if (next !== null) onChange([...history, next]) }}>{t('freePool.next')}</Button>
      </div>
    </div>
  )
}

export default function FreePool() {
  const { t } = useTranslation()
  return (
    <div className="space-y-6">
      <PageHeader title={t('freePool.title')} description={t('freePool.description')} />
      <p className="rounded-xl border bg-muted/40 p-4 text-sm leading-relaxed text-muted-foreground">{t('freePool.boundary')}</p>
      <MintWorkersSetting />
      <AccountsSection />
      <TicketsSection />
    </div>
  )
}

function MintWorkersSetting() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [workers, setWorkers] = useState('2')
  const [validationAttempts, setValidationAttempts] = useState('15')
  const [failureStrikes, setFailureStrikes] = useState('3')
  const [proxyTemplate, setProxyTemplate] = useState('')
  const [countries, setCountries] = useState('')
  const [exclusive, setExclusive] = useState(false)
  const [firstTokenTimeout, setFirstTokenTimeout] = useState('45')
  const [firstTokenStrikes, setFirstTokenStrikes] = useState('1')
  const [switchRestStrikes, setSwitchRestStrikes] = useState('8')
  const [spareDelay, setSpareDelay] = useState('30')
  const [probeConcurrency, setProbeConcurrency] = useState('1')
  const [maxProbeConcurrency, setMaxProbeConcurrency] = useState(16)
  const [maxSpareDelay, setMaxSpareDelay] = useState(600)
  const [maxFirstTokenStrikes, setMaxFirstTokenStrikes] = useState(16)
  const [maxSwitchRest, setMaxSwitchRest] = useState(64)
  const [max, setMax] = useState(64)
  const [maxAttempts, setMaxAttempts] = useState(64)
  const [maxStrikes, setMaxStrikes] = useState(64)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)
  const load = useCallback((signal: AbortSignal) => api.getFreePoolSettings(signal), [])
  const { state, reload } = useResource(load)

  useEffect(() => {
    if (state.kind === 'ready') {
      setWorkers(String(state.data.mint_workers))
      setValidationAttempts(String(state.data.validation_attempts))
      setFailureStrikes(String(state.data.mint_failure_strikes || 3))
      setProxyTemplate(state.data.proxy_template)
      setCountries(state.data.countries.join(','))
      setExclusive(state.data.exclusive_tickets)
      setFirstTokenTimeout(String(state.data.first_token_timeout_seconds || 45))
      setFirstTokenStrikes(String(state.data.first_token_strikes || 1))
      setSwitchRestStrikes(String(state.data.switch_rest_strikes || 8))
      setSpareDelay(String(state.data.spare_delay_seconds ?? 30))
      setProbeConcurrency(String(state.data.probe_concurrency || 1))
      setMaxProbeConcurrency(state.data.max_probe_concurrency || 16)
      setMaxSpareDelay(state.data.max_spare_delay_seconds || 600)
      setMaxFirstTokenStrikes(state.data.max_first_token_strikes || 16)
      setMaxSwitchRest(state.data.max_switch_rest_strikes || 64)
      setMax(state.data.max_mint_workers)
      setMaxAttempts(state.data.max_validation_attempts)
      setMaxStrikes(state.data.max_mint_failure_strikes || 64)
    }
  }, [state])

  async function clearClaims() {
    const accepted = await confirm({
      title: t('freePool.clearClaimsTitle'),
      description: t('freePool.clearClaimsHint'),
      confirmText: t('freePool.clearClaims'),
      tone: 'destructive',
      confirmVariant: 'destructive',
    })
    if (!accepted) return
    setClearing(true)
    try {
      const cleared = await api.clearFreePoolClaimRecords()
      showToast(t('freePool.claimsCleared', cleared))
    } catch (error) {
      showToast(t(errorKey(error)), 'error')
    } finally {
      setClearing(false)
    }
  }

  async function save() {
    const count = Number(workers)
    const attempts = Number(validationAttempts)
    const strikes = Number(failureStrikes)
    const timeout = Number(firstTokenTimeout)
    const tokenStrikes = Number(firstTokenStrikes)
    const restStrikes = Number(switchRestStrikes)
    const spare = Number(spareDelay)
    const probes = Number(probeConcurrency)
    if (!Number.isInteger(count) || count < 1 || count > max || !Number.isInteger(attempts) || attempts < 1 || attempts > maxAttempts || !Number.isInteger(strikes) || strikes < 1 || strikes > maxStrikes || !Number.isInteger(timeout) || timeout < 5 || timeout > 600 || !Number.isInteger(tokenStrikes) || tokenStrikes < 1 || tokenStrikes > maxFirstTokenStrikes || !Number.isInteger(restStrikes) || restStrikes < 1 || restStrikes > maxSwitchRest || !Number.isInteger(spare) || spare < 0 || spare > maxSpareDelay || !Number.isInteger(probes) || probes < 1 || probes > maxProbeConcurrency) {
      showToast(t('freePool.workerInvalid', { max, attempts: maxAttempts }), 'error')
      return
    }
    setSaving(true)
    try {
      const saved = await api.updateFreePoolSettings({
        mint_workers: count,
        validation_attempts: attempts,
        mint_failure_strikes: strikes,
        proxy_template: proxyTemplate.trim(),
        countries: countries.split(',').map((item) => item.trim()).filter(Boolean),
        exclusive_tickets: exclusive,
        first_token_timeout_seconds: timeout,
        first_token_strikes: tokenStrikes,
        switch_rest_strikes: restStrikes,
        spare_delay_seconds: spare,
        probe_concurrency: probes,
      })
      setWorkers(String(saved.mint_workers))
      setValidationAttempts(String(saved.validation_attempts))
      setFailureStrikes(String(saved.mint_failure_strikes))
      setProxyTemplate(saved.proxy_template)
      setCountries(saved.countries.join(','))
      setExclusive(saved.exclusive_tickets)
      setFirstTokenTimeout(String(saved.first_token_timeout_seconds))
      setFirstTokenStrikes(String(saved.first_token_strikes))
      setSwitchRestStrikes(String(saved.switch_rest_strikes))
      setSpareDelay(String(saved.spare_delay_seconds ?? 30))
      setProbeConcurrency(String(saved.probe_concurrency || 1))
      showToast(t('freePool.workerSaved'))
      reload()
    } catch (error) {
      showToast(t(errorKey(error)), 'error')
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardContent className="grid gap-3 p-4 sm:grid-cols-[8rem_8rem_minmax(0,1fr)_10rem_auto] sm:items-end sm:p-6">
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-workers">
          {t('freePool.workers')}
          <Input id="free-pool-workers" inputMode="numeric" value={workers} disabled={saving || state.kind === 'loading'} onChange={(event) => setWorkers(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-attempts">
          {t('freePool.validationAttempts')}
          <Input id="free-pool-attempts" inputMode="numeric" value={validationAttempts} disabled={saving || state.kind === 'loading'} onChange={(event) => setValidationAttempts(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-strikes">
          {t('freePool.failureStrikes')}
          <Input id="free-pool-strikes" inputMode="numeric" value={failureStrikes} disabled={saving || state.kind === 'loading'} onChange={(event) => setFailureStrikes(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-proxy">
          {t('freePool.proxyTemplate')}
          <Input id="free-pool-proxy" value={proxyTemplate} placeholder="socks5://user-region-{XX}:pass@host:3000" disabled={saving || state.kind === 'loading'} onChange={(event) => setProxyTemplate(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-countries">
          {t('freePool.countries')}
          <Input id="free-pool-countries" value={countries} placeholder="RANDOM" disabled={saving || state.kind === 'loading'} onChange={(event) => setCountries(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-first-token">
          {t('freePool.firstTokenTimeout')}
          <Input id="free-pool-first-token" inputMode="numeric" value={firstTokenTimeout} disabled={saving || state.kind === 'loading'} onChange={(event) => setFirstTokenTimeout(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-first-token-strikes">
          {t('freePool.firstTokenStrikes')}
          <Input id="free-pool-first-token-strikes" inputMode="numeric" value={firstTokenStrikes} disabled={saving || state.kind === 'loading'} onChange={(event) => setFirstTokenStrikes(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-switch-rest">
          {t('freePool.switchRestStrikes')}
          <Input id="free-pool-switch-rest" inputMode="numeric" value={switchRestStrikes} disabled={saving || state.kind === 'loading'} onChange={(event) => setSwitchRestStrikes(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-probe-concurrency">
          {t('freePool.probeConcurrency')}
          <Input id="free-pool-probe-concurrency" inputMode="numeric" value={probeConcurrency} disabled={saving || state.kind === 'loading'} onChange={(event) => setProbeConcurrency(event.target.value)} />
        </label>
        <label className="space-y-1.5 text-sm font-medium" htmlFor="free-pool-spare-delay">
          {t('freePool.spareDelay')}
          <Input id="free-pool-spare-delay" inputMode="numeric" value={spareDelay} disabled={saving || state.kind === 'loading'} onChange={(event) => setSpareDelay(event.target.value)} />
        </label>
        <label className="flex items-center justify-between gap-3 text-sm font-medium sm:col-span-2" htmlFor="free-pool-exclusive">
          {t('freePool.exclusiveTickets')}
          <Switch id="free-pool-exclusive" checked={exclusive} disabled={saving || state.kind === 'loading'} onCheckedChange={setExclusive} />
        </label>
        <Button type="button" disabled={saving || clearing || state.kind !== 'ready'} onClick={() => void save()}>{t(saving ? 'freePool.saving' : 'freePool.saveWorkers')}</Button>
        <Button type="button" variant="destructive" disabled={saving || clearing || state.kind !== 'ready'} onClick={() => void clearClaims()}>{t(clearing ? 'freePool.saving' : 'freePool.clearClaims')}</Button>
        <p className="text-xs text-muted-foreground sm:col-span-5">{t('freePool.proxyHintShared', { max })} {t('freePool.validationAttemptsHint', { max: maxAttempts })} {t('freePool.failureStrikesHint', { max: maxStrikes })} {t('freePool.exclusiveTicketsHint')} {t('freePool.firstTokenHint', { seconds: firstTokenTimeout, max: maxFirstTokenStrikes })} {t('freePool.switchRestStrikesHint', { max: maxSwitchRest })} {t('freePool.spareDelayHint', { max: maxSpareDelay })} {t('freePool.probeConcurrencyHint', { max: maxProbeConcurrency })}</p>
      </CardContent>
      {confirmDialog}
    </Card>
  )
}

function AccountsSection() {
  const { t, i18n } = useTranslation()
  const { showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [status, setStatus] = useState<FreePoolAccountStatus | ''>('')
  const [history, setHistory] = useState<number[]>([0])
  const [importing, setImporting] = useState(false)
  const [busy, setBusy] = useState(false)
  const mutationLock = useRef(false)
  const beforeID = history[history.length - 1]
  const load = useCallback((signal: AbortSignal) => api.listFreePoolAccounts({ before_id: beforeID, status }, signal), [beforeID, status])
  const { state, reload } = useResource(load)
  const locked = busy || importing

  function refresh() {
    setHistory([0])
    reload()
  }

  async function setAll(next: FreePoolAccountStatus) {
    if (mutationLock.current) return
    mutationLock.current = true
    setBusy(true)
    try {
      const result = await api.setAllFreePoolAccountStatus(next)
      showToast(t(next === 'active' ? 'freePool.enabledAll' : 'freePool.disabledAll', { count: result.updated }))
      refresh()
    } catch (error) {
      showToast(t(errorKey(error)), 'error')
    } finally {
      mutationLock.current = false
      setBusy(false)
    }
  }

  async function mutate(account: FreePoolAccountSummary, remove: boolean) {
    if (mutationLock.current) return
    mutationLock.current = true
    setBusy(true)
    try {
      if (remove) {
        const accepted = await confirm({
          title: t('freePool.deleteTitle'),
          description: t('freePool.deleteHint', { name: account.name, id: account.id }),
          confirmText: t('freePool.delete'),
          tone: 'destructive',
          confirmVariant: 'destructive',
        })
        if (!accepted) return
        await api.deleteFreePoolAccount(account.id)
        showToast(t('freePool.deleted'))
      } else {
        await api.setFreePoolAccountStatus(account.id, account.status === 'active' ? 'disabled' : 'active')
        showToast(t('freePool.updated'))
      }
      refresh()
    } catch (error) {
      showToast(t(errorKey(error)), 'error')
    } finally {
      mutationLock.current = false
      setBusy(false)
    }
  }

  return (
    <Card>
      <CardContent className="space-y-4 p-4 sm:p-6">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h3 className="text-lg font-semibold">{t('freePool.accounts')}</h3>
          <div className="flex flex-wrap gap-2">
            <Select
              aria-label={t('freePool.status')}
              className="w-auto min-w-36"
              value={status}
              disabled={locked || state.kind === 'loading'}
              options={[{ value: '', label: t('freePool.allStatuses') }, ...FREE_POOL_ACCOUNT_STATUSES.map((value) => ({ value, label: t(`freePool.accountStatus.${value}`) }))]}
              onValueChange={(value) => {
                setStatus(value === '' ? '' : FREE_POOL_ACCOUNT_STATUSES.find((item) => item === value) ?? status)
                setHistory([0])
              }}
            />
            <Button type="button" variant="outline" disabled={locked || state.kind === 'loading'} onClick={refresh}>{t('freePool.refresh')}</Button>
            <Button type="button" variant="outline" disabled={locked} onClick={() => void setAll('active')}>{t('freePool.enableAll')}</Button>
            <Button type="button" variant="outline" disabled={locked} onClick={() => void setAll('disabled')}>{t('freePool.disableAll')}</Button>
            <Button type="button" disabled={locked || state.kind !== 'ready'} onClick={() => setImporting(true)}>{t('freePool.importAccount')}</Button>
          </div>
        </div>
        <StateShell
          loading={state.kind === 'loading'}
          error={state.kind === 'error' ? t(state.errorKey) : undefined}
          onRetry={reload}
          isEmpty={state.kind === 'ready' && state.data.items.length === 0}
          emptyTitle={t('freePool.empty')}
          emptyDescription={t('freePool.emptyHint')}
        >
          {state.kind === 'ready' && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('freePool.name')}</TableHead>
                  <TableHead>{t('freePool.status')}</TableHead>
                  <TableHead>{t('freePool.proxy')}</TableHead>
                  <TableHead>{t('freePool.accountHealth')}</TableHead>
                  <TableHead>{t('freePool.timestamps')}</TableHead>
                  <TableHead>{t('freePool.operations')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {state.data.items.map((account) => (
                  <TableRow key={account.id}>
                    <TableCell>{account.id}</TableCell>
                    <TableCell className="max-w-64 whitespace-normal break-words">{account.name}</TableCell>
                    <TableCell>{t(`freePool.accountStatus.${account.status}`)}</TableCell>
                    <TableCell><code>{account.proxy_url || '—'}</code></TableCell>
                    <TableCell>
                      <div>{t('freePool.cooldown')}: {formatTime(account.cooldown_until, i18n.language)}</div>
                      <div className={account.has_error ? 'text-destructive' : ''}>{t('freePool.hasError')}: {t(account.has_error ? 'freePool.yes' : 'freePool.no')}</div>
                    </TableCell>
                    <TableCell>
                      <div>{t('freePool.createdAt')}: {formatTime(account.created_at, i18n.language)}</div>
                      <div>{t('freePool.updatedAt')}: {formatTime(account.updated_at, i18n.language)}</div>
                    </TableCell>
                    <TableCell>
                      <div className="flex gap-2">
                        <Button type="button" size="sm" variant="outline" disabled={locked} onClick={() => void mutate(account, false)}>{t(account.status === 'active' ? 'freePool.disable' : 'freePool.enable')}</Button>
                        <Button type="button" size="sm" variant="destructive" disabled={locked} onClick={() => void mutate(account, true)}>{t('freePool.delete')}</Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </StateShell>
        {state.kind === 'ready' && <CursorPager history={history} next={state.data.next_before_id} count={state.data.items.length} disabled={locked} onChange={setHistory} />}
        {importing && <ImportAccountModal onClose={() => setImporting(false)} onCreated={() => { setImporting(false); setStatus(''); setHistory([0]); reload() }} />}
        {confirmDialog}
      </CardContent>
    </Card>
  )
}

function ImportAccountModal({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [draft, setDraft] = useState(newFreePoolAccountDraft)
  const [file, setFile] = useState<File | null>(null)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const submitLock = useRef(false)

  function close() {
    if (submitLock.current) return
    setDraft(newFreePoolAccountDraft())
    onClose()
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (submitLock.current) return
    setFormError(null)
    submitLock.current = true
    setSaving(true)
    try {
      if (file) {
        const imported = await api.importFreePoolAccounts(file, draft.status)
        setFile(null)
        showToast(t('freePool.imported', { created: imported.created, skipped: imported.skipped }))
      } else {
        const result = validateFreePoolAccount(draft)
        if (!result.ok) {
          setFormError(result.errorKey)
          return
        }
        await api.createFreePoolAccount(result.value)
        showToast(t('freePool.created'))
      }
      setDraft(newFreePoolAccountDraft())
      onCreated()
    } catch (error) {
      setFormError(errorKey(error))
    } finally {
      submitLock.current = false
      setSaving(false)
    }
  }

  return (
    <Modal
      show
      title={t('freePool.importAccount')}
      onClose={close}
      contentClassName="sm:max-w-[680px]"
      showCloseButton={!saving}
      footer={(
        <>
          <Button type="button" variant="outline" disabled={saving} onClick={close}>{t('freePool.cancel')}</Button>
          <Button type="submit" form="free-pool-import-form" disabled={saving}>{t(saving ? 'freePool.saving' : 'freePool.create')}</Button>
        </>
      )}
    >
      <form id="free-pool-import-form" className="space-y-4" onSubmit={(event) => void submit(event)} autoComplete="off" noValidate>
        <label className="block space-y-1.5 text-sm font-medium" htmlFor="free-pool-name">
          {t('freePool.name')}
          <Input id="free-pool-name" required={!file} disabled={saving} value={draft.name} onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))} />
        </label>
        <label className="block space-y-1.5 text-sm font-medium" htmlFor="free-pool-status">
          {t('freePool.status')}
          <Select
            id="free-pool-status"
            value={draft.status}
            disabled={saving}
            options={FREE_POOL_ACCOUNT_STATUSES.map((value) => ({ value, label: t(`freePool.accountStatus.${value}`) }))}
            onValueChange={(value) => {
              const next = FREE_POOL_ACCOUNT_STATUSES.find((item) => item === value)
              if (next) setDraft((current) => ({ ...current, status: next }))
            }}
          />
          <span className="block text-xs font-normal text-muted-foreground">{t('freePool.statusHint')}</span>
        </label>
        <label className="block space-y-1.5 text-sm font-medium" htmlFor="free-pool-proxy">
          {t('freePool.proxy')}
          <Input id="free-pool-proxy" type="password" autoComplete="new-password" disabled={saving} value={draft.proxy_url} onChange={(event) => setDraft((current) => ({ ...current, proxy_url: event.target.value }))} />
          <span className="block text-xs font-normal text-muted-foreground">{t('freePool.proxyHint')}</span>
        </label>
        <label className="block space-y-1.5 text-sm font-medium" htmlFor="free-pool-file">
          {t('freePool.importFile')}
          <Input id="free-pool-file" type="file" accept="application/json,.json" disabled={saving} onChange={(event) => setFile(event.target.files?.[0] ?? null)} />
          <span className="block text-xs font-normal leading-relaxed text-muted-foreground">{t('freePool.importFileHint')}</span>
        </label>
        <label className="block space-y-1.5 text-sm font-medium" htmlFor="free-pool-credentials">
          {t('freePool.credentials')}
          <textarea id="free-pool-credentials" required={!file} rows={8} disabled={saving} value={draft.credentials} autoComplete="off" spellCheck={false} className="w-full rounded-lg border border-input bg-background p-3 font-mono text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50" onChange={(event) => setDraft((current) => ({ ...current, credentials: event.target.value }))} />
          <span className="block text-xs font-normal leading-relaxed text-muted-foreground">{t('freePool.credentialsHint')}</span>
        </label>
        {formError && <p role="alert" className="text-sm text-destructive">{t(formError)}</p>}
      </form>
    </Modal>
  )
}

function TicketsSection() {
  const { t, i18n } = useTranslation()
  const { showToast } = useToast()
  const [status, setStatus] = useState<FreePoolTicketStatus | ''>('')
  const [sourceText, setSourceText] = useState('')
  const [sourceID, setSourceID] = useState(0)
  const [history, setHistory] = useState<number[]>([0])
  const [selectedTicketID, setSelectedTicketID] = useState<number | null>(null)
  const beforeID = history[history.length - 1]
  const load = useCallback((signal: AbortSignal) => api.listFreePoolTickets({ before_id: beforeID, status, source_account_id: sourceID }, signal), [beforeID, sourceID, status])
  const { state, reload } = useResource(load)

  function refresh() {
    setSelectedTicketID(null)
    setHistory([0])
    reload()
  }

  function applySource() {
    const result = parseFreePoolSourceID(sourceText)
    if (!result.ok) {
      showToast(t(result.errorKey), 'error')
      return
    }
    setSourceID(result.value)
    setSelectedTicketID(null)
    setHistory([0])
  }

  return (
    <Card>
      <CardContent className="space-y-4 p-4 sm:p-6">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h3 className="text-lg font-semibold">{t('freePool.tickets')}</h3>
          <Button type="button" variant="outline" disabled={state.kind === 'loading'} onClick={refresh}>{t('freePool.refresh')}</Button>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Select
            aria-label={t('freePool.status')}
            className="w-auto min-w-36"
            value={status}
            disabled={state.kind === 'loading'}
            options={[{ value: '', label: t('freePool.allStatuses') }, ...FREE_POOL_TICKET_STATUSES.map((value) => ({ value, label: t(`freePool.ticketStatus.${value}`) }))]}
            onValueChange={(value) => {
              setStatus(value === '' ? '' : FREE_POOL_TICKET_STATUSES.find((item) => item === value) ?? status)
              setSelectedTicketID(null)
              setHistory([0])
            }}
          />
          <Input className="w-full sm:w-64" aria-label={t('freePool.sourceFilter')} placeholder={t('freePool.sourceFilter')} inputMode="numeric" value={sourceText} disabled={state.kind === 'loading'} onChange={(event) => setSourceText(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); applySource() } }} />
          <Button type="button" variant="outline" disabled={state.kind === 'loading'} onClick={applySource}>{t('freePool.apply')}</Button>
        </div>
        <StateShell
          loading={state.kind === 'loading'}
          error={state.kind === 'error' ? t(state.errorKey) : undefined}
          onRetry={reload}
          isEmpty={state.kind === 'ready' && state.data.items.length === 0}
          emptyTitle={t('freePool.empty')}
          emptyDescription={t('freePool.emptyHint')}
        >
          {state.kind === 'ready' && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>ID</TableHead>
                  <TableHead>{t('freePool.sourceAccount')}</TableHead>
                  <TableHead>{t('freePool.mintModel')}</TableHead>
                  <TableHead>{t('freePool.source')}</TableHead>
                  <TableHead>{t('freePool.issuedAt')}</TableHead>
                  <TableHead>{t('freePool.hardExpiry')}</TableHead>
                  <TableHead>{t('freePool.status')}</TableHead>
                  <TableHead>{t('freePool.quarantine')}</TableHead>
                  <TableHead>{t('freePool.operations')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {state.data.items.map((ticket) => (
                  <TableRow key={ticket.id}>
                    <TableCell>{ticket.id}</TableCell>
                    <TableCell>{ticket.source_account_id}</TableCell>
                    <TableCell><code>{ticket.mint_model}</code></TableCell>
                    <TableCell>
                      <div>{t('freePool.gateway')}: {ticket.source_gateway}</div>
                      <div>{t('freePool.colo')}: {ticket.source_colo}</div>
                    </TableCell>
                    <TableCell>{formatTime(ticket.issued_at, i18n.language)}</TableCell>
                    <TableCell>
                      <div>{formatTime(ticket.hard_expires_at, i18n.language)}</div>
                      <div className={ticket.hard_expired ? 'text-destructive' : 'text-muted-foreground'}>{t(ticket.hard_expired ? 'freePool.hardExpired' : 'freePool.belowHardLimit')}</div>
                    </TableCell>
                    <TableCell>{t(`freePool.ticketStatus.${ticket.status}`)}</TableCell>
                    <TableCell>{t(ticket.has_quarantine_reason ? 'freePool.yes' : 'freePool.no')}</TableCell>
                    <TableCell><Button type="button" size="sm" variant="outline" onClick={() => setSelectedTicketID(ticket.id)}>{t('freePool.probeSummary')}</Button></TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </StateShell>
        {state.kind === 'ready' && (
          <CursorPager history={history} next={state.data.next_before_id} count={state.data.items.length} onChange={(nextHistory) => { setSelectedTicketID(null); setHistory(nextHistory) }} />
        )}
        {selectedTicketID !== null && <ProbeSummaryModal key={selectedTicketID} ticketID={selectedTicketID} onClose={() => setSelectedTicketID(null)} />}
      </CardContent>
    </Card>
  )
}

function ProbeSummaryModal({ ticketID, onClose }: { ticketID: number; onClose: () => void }) {
  const { t, i18n } = useTranslation()
  const load = useCallback((signal: AbortSignal) => api.getFreePoolProbeSummary(ticketID, signal), [ticketID])
  const { state, reload } = useResource(load)
  return (
    <Modal
      show
      title={t('freePool.probeTitle', { id: ticketID })}
      onClose={onClose}
      contentClassName="sm:max-w-[960px]"
      footer={(
        <>
          <Button type="button" variant="outline" disabled={state.kind === 'loading'} onClick={reload}>{t('freePool.refresh')}</Button>
          <Button type="button" onClick={onClose}>{t('freePool.close')}</Button>
        </>
      )}
    >
      <div className="space-y-4">
        <p className="text-sm leading-relaxed text-muted-foreground">{t('freePool.probeHint')}</p>
        {state.kind === 'ready' && <p className="text-sm font-medium">{t('freePool.probeTotal', { total: state.data.total })}</p>}
        <StateShell
          loading={state.kind === 'loading'}
          error={state.kind === 'error' ? t(state.errorKey) : undefined}
          onRetry={reload}
          isEmpty={state.kind === 'ready' && state.data.items.length === 0}
          emptyTitle={t('freePool.empty')}
          emptyDescription={t('freePool.emptyHint')}
        >
          {state.kind === 'ready' && (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('freePool.stage')}</TableHead>
                  <TableHead>{t('freePool.result')}</TableHead>
                  <TableHead>{t('freePool.errorCode')}</TableHead>
                  <TableHead>{t('freePool.count')}</TableHead>
                  <TableHead>{t('freePool.newStateCount')}</TableHead>
                  <TableHead>{t('freePool.lastObserved')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {state.data.items.map((item) => (
                  <TableRow key={`${item.stage}:${item.status}:${item.error_code}`}>
                    <TableCell>{t(`freePool.probeStage.${item.stage}`)}</TableCell>
                    <TableCell>{t(`freePool.probeStatus.${item.status}`)}</TableCell>
                    <TableCell><code>{item.error_code || '—'}</code></TableCell>
                    <TableCell>{item.count}</TableCell>
                    <TableCell>{item.new_state_count}</TableCell>
                    <TableCell>{formatTime(item.last_created_at, i18n.language)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </StateShell>
      </div>
    </Modal>
  )
}
