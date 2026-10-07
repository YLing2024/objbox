export type AuthMode = 'builtin' | 'sso'

export interface Account {
  name: string
  ak: string
  sk: string
  root: string
  usageBytes: number
  status: 'enabled' | 'disabled' | 'readonly'
  note: string
  quotaBytes: number
  readonly: boolean
  disabled: boolean
  bucket: string
  autoCreateBucket: boolean
  bucketExists: boolean
}

export interface Overview {
  authMode: AuthMode
  accounts: number
  totalUsageBytes: number
  version: string
}

export interface AccountList {
  accounts: Account[]
  endpoint?: string
  region?: string
  pathStyle?: boolean
}

export interface BucketInfo {
  name: string
  objects: number
  bytes: number
  isDefault: boolean
}

export interface OneTimeCredential {
  name: string
  ak: string
  sk: string
  root?: string
  endpoint?: string
  region?: string
  pathStyle?: boolean
  note?: string
  bucket?: string
  autoCreateBucket?: boolean
}

export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    credentials: 'same-origin',
    ...init,
  })
  const text = await resp.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }
  if (!resp.ok) {
    const message =
      data && typeof data === 'object' && 'error' in data
        ? String((data as { error: unknown }).error)
        : `请求失败（HTTP ${resp.status}）`
    throw new ApiError(resp.status, message)
  }
  return data as T
}

function jsonInit(method: string, body?: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  }
}

export const api = {
  overview: () => request<Overview>('/api/admin/overview'),
  login: (password: string) =>
    request<{ ok: boolean }>('/api/admin/login', jsonInit('POST', { password })),
  logout: () => request<{ ok: boolean }>('/api/admin/logout', jsonInit('POST', {})),
  accounts: () => request<AccountList>('/api/admin/accounts'),
  reveal: (name: string) =>
    request<{ accounts: Account[] }>(`/api/admin/accounts?reveal=${encodeURIComponent(name)}`),
  listBuckets: (name: string) =>
    request<{ buckets: BucketInfo[] }>(
      `/api/admin/accounts/${encodeURIComponent(name)}/buckets`,
    ),
  createBucket: (name: string, bucket: string) =>
    request<{ name: string }>(
      `/api/admin/accounts/${encodeURIComponent(name)}/buckets`,
      jsonInit('POST', { name: bucket }),
    ),
  deleteBucket: (name: string, bucket: string) =>
    request<{ name: string; message: string }>(
      `/api/admin/accounts/${encodeURIComponent(name)}/buckets/${encodeURIComponent(bucket)}`,
      jsonInit('DELETE'),
    ),
  updateBucket: (
    name: string,
    patch: { bucket?: string; autoCreateBucket?: boolean },
  ) =>
    request<Account>(
      `/api/admin/accounts/${encodeURIComponent(name)}/bucket`,
      jsonInit('PATCH', patch),
    ),
  create: (name: string, note: string, bucket?: string, autoCreateBucket?: boolean) =>
    request<OneTimeCredential>(
      '/api/admin/accounts',
      jsonInit('POST', { name, note, bucket: bucket || undefined, autoCreateBucket }),
    ),
  rotate: (name: string) =>
    request<{ name: string; sk: string }>(
      `/api/admin/accounts/${encodeURIComponent(name)}/rotate`,
      jsonInit('POST', {}),
    ),
  setDisabled: (name: string, disabled: boolean) =>
    request<unknown>(
      `/api/admin/accounts/${encodeURIComponent(name)}/${disabled ? 'disable' : 'enable'}`,
      jsonInit('POST', {}),
    ),
  update: (name: string, patch: { note?: string; quotaBytes?: number }) =>
    request<Account>(`/api/admin/accounts/${encodeURIComponent(name)}`, jsonInit('PATCH', patch)),
  remove: (name: string) =>
    request<{ message: string }>(`/api/admin/accounts/${encodeURIComponent(name)}`, jsonInit('DELETE')),
}
