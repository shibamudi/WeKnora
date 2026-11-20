/** Resolve the public base URL (origin + optional subpath) used in embed
 *  iframe / widget URLs. Configured EMBED_BASE_URL takes precedence
 *  (dedicated embed domain); otherwise the current origin + Vite base
 *  path (e.g. /weknora) is used so subpath deployments produce
 *  routable URLs. */
export function resolveEmbedBaseUrl(): string {
  if (typeof window === 'undefined') return ''
  const candidates = [
    (window as Window & { __RUNTIME_CONFIG__?: { EMBED_BASE_URL?: string } }).__RUNTIME_CONFIG__?.EMBED_BASE_URL,
    import.meta.env.VITE_EMBED_BASE_URL,
  ]
  for (const candidate of candidates) {
    const trimmed = candidate?.trim()
    if (!trimmed) continue
    try {
      const u = new URL(trimmed, window.location.href)
      if (u.protocol !== 'http:' && u.protocol !== 'https:') continue
      const path = u.pathname !== '/' ? u.pathname.replace(/\/+$/, '') : ''
      return u.origin + path
    } catch {
      // try next candidate
    }
  }
  const base = (import.meta.env?.BASE_URL || '/').replace(/\/+$/, '')
  return window.location.origin + base
}
