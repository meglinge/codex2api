const usd = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
})

export function formatImageStudioQuota(amount: number | null | undefined): string {
  if (amount == null || !Number.isFinite(amount)) return '—'
  if (amount > 0 && amount < 0.0001) return '< $0.0001'
  return usd.format(Math.max(0, amount))
}
