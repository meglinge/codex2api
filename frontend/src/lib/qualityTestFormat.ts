// Pretty-print generated markup for the source view. Prettier's browser build and
// its HTML/CSS/JS plugins load on demand, so the page bundle stays untouched.
export const QUALITY_TEST_FORMAT_LIMIT = 200 * 1024

const cache = new Map<string, string>()
const CACHE_SIZE = 20

export async function formatQualitySource(source: string): Promise<string> {
  if (!source.trim() || source.length > QUALITY_TEST_FORMAT_LIMIT) return source
  const cached = cache.get(source)
  if (cached !== undefined) return cached
  const [prettier, html, postcss, babel, estree] = await Promise.all([
    import('prettier/standalone'),
    import('prettier/plugins/html'),
    import('prettier/plugins/postcss'),
    import('prettier/plugins/babel'),
    import('prettier/plugins/estree'),
  ])
  let formatted = source
  try {
    formatted = await prettier.format(source, { parser: 'html', plugins: [html.default, postcss.default, babel.default, estree.default], printWidth: 120, tabWidth: 2, htmlWhitespaceSensitivity: 'css', bracketSameLine: true })
  } catch {
    return source
  }
  if (cache.size >= CACHE_SIZE) { const first = cache.keys().next().value; if (first !== undefined) cache.delete(first) }
  cache.set(source, formatted)
  return formatted
}
