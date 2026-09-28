import { useEffect, useRef, useState } from 'react'
import { api } from '../api'
import type { AccountRow } from '../types'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { Switch } from '@/components/ui/switch'
import { isCodexTurnStateAccount } from './CodexTurnStateBadge'

export default function AccountTicketSwitch({ account, onSaved }: { account: AccountRow; onSaved: () => void }) {
  const { showToast } = useToast()
  const [checked, setChecked] = useState(account.use_tickets ?? false)
  const [saving, setSaving] = useState(false)
  const inFlight = useRef(false)

  useEffect(() => {
    setChecked(account.use_tickets ?? false)
  }, [account.id, account.use_tickets])

  if (!isCodexTurnStateAccount(account) || account.agent_identity) return null

  const save = async (value: boolean) => {
    if (inFlight.current) return
    inFlight.current = true
    setSaving(true)
    try {
      const result = await api.updateAccountUseTickets(account.id, value)
      setChecked(result.use_tickets)
      showToast('票池设置已保存')
      onSaved()
    } catch (error: unknown) {
      showToast(getErrorMessage(error), 'error')
    } finally {
      inFlight.current = false
      setSaving(false)
    }
  }

  return (
    <div className="flex items-center justify-between gap-3">
      <div>
        <div className="text-xs font-semibold text-foreground">使用 Free 票池</div>
        <div className="text-[11px] text-muted-foreground">即时保存。这个账号第一次使用某张票时才双 400 验证；验证失败不发送用户请求。</div>
      </div>
      <Switch aria-label="使用 Free 票池" checked={checked} disabled={saving} onCheckedChange={(value) => void save(value)} />
    </div>
  )
}
