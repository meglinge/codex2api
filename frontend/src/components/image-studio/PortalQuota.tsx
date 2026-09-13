import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, ChevronDown, KeyRound, RefreshCw, Wallet } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { formatImageStudioQuota } from '@/lib/imageStudioQuota'
import { formatBeijingTime } from '@/utils/time'
import type { ImageStudioQuota } from '@/types'

export function PortalQuota({ maskedKey, quota, loading, error, onRefresh }: {
  maskedKey: string
  quota: ImageStudioQuota | null
  loading: boolean
  error: string
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const amount = quota?.quota_remaining === null
    ? t('imageStudioPortal.quota.unlimited')
    : formatImageStudioQuota(quota?.quota_remaining)
  const label = error ? t('imageStudioPortal.quota.unavailable') : quota
    ? quota.quota_remaining === null ? t('imageStudioPortal.quota.unlimitedLabel') : t('imageStudioPortal.quota.remainingAmount', { amount })
    : t(loading ? 'imageStudioPortal.quota.loading' : 'imageStudioPortal.quota.unavailable')
  const low = quota?.quota_remaining != null && quota.quota_limit > 0 && quota.quota_remaining / quota.quota_limit <= 0.1
  return (
    <div className="portal-key-summary">
      <Button type="button" variant="outline" className="portal-quota-trigger" data-warning={Boolean(error || low || quota?.status === 'expired')} onClick={() => { setOpen(true); onRefresh() }}>
        <KeyRound className="size-3.5" />
        <span className="portal-quota-key">{maskedKey}</span>
        <span className="portal-quota-balance">{label}</span>
        {error ? <AlertTriangle className="size-3.5" aria-label={t('imageStudioPortal.quota.refreshFailed')} /> : <ChevronDown className="size-3.5" />}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="portal-quota-dialog sm:max-w-[420px]">
          <DialogHeader>
            <DialogTitle>{t('imageStudioPortal.quota.title')}</DialogTitle>
            <DialogDescription>{t('imageStudioPortal.quota.description')}</DialogDescription>
          </DialogHeader>
          <div className="portal-quota-amount" aria-live="polite">
            <span><Wallet className="size-4" />{t(error && quota ? 'imageStudioPortal.quota.lastKnownRemaining' : 'imageStudioPortal.quota.remaining')}</span>
            <strong>{quota ? amount : '—'}</strong>
            {quota?.status === 'expired' || quota?.status === 'quota_exhausted' ? <p className="text-destructive">{t(`imageStudioPortal.quota.${quota.status}`)}</p> : null}
          </div>
          {quota ? (
            <dl className="portal-quota-details">
              <div><dt>{t('imageStudioPortal.quota.limit')}</dt><dd>{quota.quota_remaining === null ? t('imageStudioPortal.quota.unlimited') : formatImageStudioQuota(quota.quota_limit)}</dd></div>
              <div><dt>{t('imageStudioPortal.quota.used')}</dt><dd>{formatImageStudioQuota(quota.quota_used)}</dd></div>
              <div><dt>{t('imageStudioPortal.quota.expiresAt')}</dt><dd>{quota.expires_at ? formatBeijingTime(quota.expires_at) : t('imageStudioPortal.quota.neverExpires')}</dd></div>
            </dl>
          ) : null}
          {error ? <p role="alert" className="portal-quota-error">{t('imageStudioPortal.quota.refreshFailed')} · {error}</p> : null}
          <p className="portal-quota-note">{t('imageStudioPortal.quota.settlementHint')}</p>
          <Button type="button" variant="outline" onClick={onRefresh} disabled={loading}>
            <RefreshCw className={loading ? 'size-4 animate-spin' : 'size-4'} />
            {t(loading ? 'imageStudioPortal.quota.refreshing' : 'imageStudioPortal.quota.refresh')}
          </Button>
        </DialogContent>
      </Dialog>
    </div>
  )
}
