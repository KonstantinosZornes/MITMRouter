// 极简 API 客户端：统一 JSON/错误/401 跳转。
export class ApiError extends Error {
  code: string
  status: number
  constructor(code: string, msg: string, status: number) {
    super(msg); this.code = code; this.status = status
  }
}

export async function api<T = any>(path: string, opts?: { method?: string; body?: any }): Promise<T> {
  const r = await fetch(path, {
    method: opts?.method ?? 'GET',
    headers: opts?.body ? { 'content-type': 'application/json' } : undefined,
    credentials: 'include',
    body: opts?.body ? JSON.stringify(opts.body) : undefined,
  })
  if (r.status === 401 && !path.startsWith('/api/auth/login')) {
    location.hash = '#/login'
    throw new ApiError('unauthorized', '请先登录', 401)
  }
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new ApiError(j?.error?.code ?? 'http_' + r.status, j?.error?.message ?? r.statusText, r.status)
  return j as T
}

export interface SettingsDTO {
  ingress_url: string; ingress_url_auth: string
  listen_tls_cert: string; listen_tls_key: string
  admin_tls_cert: string; admin_tls_key: string
  listen_auth: string; default_upstream: string
  no_marker_policy: string; marker_path_parts: string[]; marker_headers: string[]
  hash_salt: string; sid_len: number; session_ttl_min: number
  salt_rotate_failure_threshold: number
  log_retention_days: number; metrics_enabled: boolean
  sync_empty_clear_threshold: number
  block_private_targets: boolean
  acl_whitelist: string[]; acl_blacklist: string[]
}
export interface UpstreamDTO {
  id: number; name: string; platform: string; base_url: string
  inject?: any; enabled: boolean; default: boolean
}
// 账户↔出站绑定（docs/011）
export interface BindingDTO {
  platform: string; account: string
  mode: string                  // sticky | random
  egress_ids: number[]
  egress_names: string[]
}
