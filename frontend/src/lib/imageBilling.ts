export function supportsImageBilling(model: string): boolean {
  const name = model.trim().toLowerCase()
  return name.startsWith('gpt-image-') || ['grok-2-image', 'grok-2-image-1212', 'grok-imagine-image', 'grok-imagine-image-pro', 'grok-imagine-image-quality'].includes(name)
}
