import { useCallback, useEffect, useState } from 'react'
import {
  ApiError,
  api,
  type Account,
  type AuthMode,
  type OneTimeCredential,
  type Overview,
} from './api'

// ---- 小工具 ----

function useHashRoute(): [string, (r: string) => void] {
  const read = () => window.location.hash.replace(/^#/, '') || '/'
  const [route, setRoute] = useState(read)
  useEffect(() => {
    const onChange = () => setRoute(read())
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  const navigate = (r: string) => {
    if (read() !== r) {
      window.location.hash = r
    }
  }
  return [route, navigate]
}

function formatBytes(n: number): string {
  if (!n || n <= 0) {
    return '0 B'
  }
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let value = n
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i += 1
  }
  return `${i === 0 ? value : value.toFixed(1)} ${units[i]}`
}

const statusLabel: Record<Account['status'], string> = {
  enabled: '启用',
  disabled: '停用',
  readonly: '只读',
}

function CopyButton({ value, label = '复制' }: { value: string; label?: string }) {
  const [done, setDone] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setDone(true)
      window.setTimeout(() => setDone(false), 1500)
    } catch {
      // 剪贴板不可用时静默失败
    }
  }
  return (
    <button type="button" className="link" onClick={copy}>
      {done ? '已复制' : label}
    </button>
  )
}

// ---- 登录 / 未认证 ----

function LoginView({
  error,
  onSubmit,
}: {
  error: string
  onSubmit: (password: string) => void
}) {
  const [password, setPassword] = useState('')
  return (
    <div className="center">
      <form
        className="card login"
        onSubmit={(e) => {
          e.preventDefault()
          onSubmit(password)
        }}
      >
        <h1>objbox 管理</h1>
        <p className="muted">请输入管理员口令</p>
        <input
          type="password"
          value={password}
          autoFocus
          placeholder="管理员口令"
          onChange={(e) => setPassword(e.target.value)}
        />
        {error ? <p className="error">{error}</p> : null}
        <button type="submit" className="primary">
          登录
        </button>
      </form>
    </div>
  )
}

function ForbiddenView() {
  return (
    <div className="center">
      <div className="card login">
        <h1>未认证</h1>
        <p className="muted">当前为 SSO 模式，请通过网关携带 X-Auth-User 访问。</p>
        <p className="error">401 Unauthorized</p>
      </div>
    </div>
  )
}

// ---- 顶部栏 ----

function TopBar({
  mode,
  user,
  overview,
  onLogout,
}: {
  mode: AuthMode
  user: string
  overview: Overview | null
  onLogout: () => void
}) {
  return (
    <header className="topbar">
      <div className="brand">objbox 管理</div>
      <div className="stats">
        <span className="tag">{mode === 'sso' ? 'SSO' : 'builtin'}</span>
        <span>账号 {overview ? overview.accounts : 0}</span>
        <span>总用量 {formatBytes(overview ? overview.totalUsageBytes : 0)}</span>
      </div>
      <div className="right">
        {mode === 'sso' ? (
          <span className="muted">当前用户：{user || '未知'}</span>
        ) : (
          <button type="button" className="link" onClick={onLogout}>
            退出
          </button>
        )}
      </div>
    </header>
  )
}

// ---- 新增 / 编辑弹窗 ----

function AddAccountModal({
  onClose,
  onCreated,
}: {
  onClose: () => void
  onCreated: (cred: OneTimeCredential) => void
}) {
  const [name, setName] = useState('')
  const [note, setNote] = useState('')
  const [autoCreate, setAutoCreate] = useState(true)
  const [bucket, setBucket] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const valid = /^[a-z0-9-]{1,32}$/.test(name)
  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      const cred = await api.create(name, note, bucket.trim(), autoCreate)
      onCreated(cred)
    } catch (e) {
      setError(e instanceof Error ? e.message : '创建失败')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="overlay">
      <div className="card modal">
        <h2>新增账号</h2>
        <label>
          账号名（[a-z0-9-]，1-32 位）
          <input value={name} autoFocus onChange={(e) => setName(e.target.value)} />
        </label>
        <label>
          备注
          <input value={note} onChange={(e) => setNote(e.target.value)} />
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={autoCreate}
            onChange={(e) => setAutoCreate(e.target.checked)}
          />
          自动创建同名桶
        </label>
        <label>
          默认桶名（留空 = 账号名）
          <input
            value={bucket}
            disabled={!autoCreate}
            placeholder={name || '账号名'}
            onChange={(e) => setBucket(e.target.value)}
          />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <div className="actions">
          <button type="button" onClick={onClose}>
            取消
          </button>
          <button type="button" className="primary" disabled={!valid || busy} onClick={submit}>
            创建
          </button>
        </div>
      </div>
    </div>
  )
}

function EditAccountModal({
  account,
  onClose,
  onSaved,
}: {
  account: Account
  onClose: () => void
  onSaved: () => void
}) {
  const [note, setNote] = useState(account.note)
  const [quota, setQuota] = useState(String(account.quotaBytes))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async () => {
    const quotaBytes = Number(quota)
    if (!Number.isFinite(quotaBytes) || quotaBytes < 0) {
      setError('配额必须是非负整数')
      return
    }
    setBusy(true)
    setError('')
    try {
      await api.update(account.name, { note, quotaBytes })
      onSaved()
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="overlay">
      <div className="card modal">
        <h2>编辑账号 {account.name}</h2>
        <label>
          备注
          <input value={note} onChange={(e) => setNote(e.target.value)} />
        </label>
        <label>
          配额（字节，0 表示不限）
          <input value={quota} onChange={(e) => setQuota(e.target.value)} />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <div className="actions">
          <button type="button" onClick={onClose}>
            取消
          </button>
          <button type="button" className="primary" disabled={busy} onClick={submit}>
            保存
          </button>
        </div>
      </div>
    </div>
  )
}

// ---- 一次性密钥弹窗 ----

function SecretModal({
  title,
  name,
  ak,
  sk,
  endpoint,
  region,
  pathStyle,
  onClose,
}: {
  title: string
  name: string
  ak?: string
  sk: string
  endpoint?: string
  region?: string
  pathStyle?: boolean
  onClose: () => void
}) {
  return (
    <div className="overlay">
      <div className="card modal">
        <h2>{title}</h2>
        <p className="warn">关闭后不再显示，请立即保存。</p>
        <dl className="kv">
          <dt>账号</dt>
          <dd>
            {name} <CopyButton value={name} />
          </dd>
          {ak ? (
            <>
              <dt>AK</dt>
              <dd>
                <code>{ak}</code> <CopyButton value={ak} />
              </dd>
            </>
          ) : null}
          <dt>SK</dt>
          <dd>
            <code>{sk}</code> <CopyButton value={sk} />
          </dd>
          {endpoint ? (
            <>
              <dt>Endpoint</dt>
              <dd>
                <code>{endpoint}</code> <CopyButton value={endpoint} />
              </dd>
            </>
          ) : null}
          {region ? (
            <>
              <dt>Region</dt>
              <dd>
                <code>{region}</code>
              </dd>
            </>
          ) : null}
          {pathStyle !== undefined ? (
            <>
              <dt>PathStyle</dt>
              <dd>{pathStyle ? 'true' : 'false'}</dd>
            </>
          ) : null}
        </dl>
        {ak && endpoint ? (
          <div className="actions">
            <CopyButton
              label="复制连接信息"
              value={`Endpoint: ${endpoint}\nRegion: ${region ?? ''}\nPathStyle: ${pathStyle}\nAK: ${ak}\nSK: ${sk}`}
            />
          </div>
        ) : null}
        <div className="actions">
          <button type="button" className="primary" onClick={onClose}>
            我已保存
          </button>
        </div>
      </div>
    </div>
  )
}

// ---- 账号表格 ----

function AccountsView({
  accounts,
  onAdd,
  onReveal,
  onRotate,
  onToggle,
  onEdit,
  onDelete,
}: {
  accounts: Account[]
  onAdd: () => void
  onReveal: (name: string) => void
  onRotate: (name: string) => void
  onToggle: (a: Account) => void
  onEdit: (a: Account) => void
  onDelete: (name: string) => void
}) {
  return (
    <main className="content">
      <div className="toolbar">
        <h2>账号列表</h2>
        <button type="button" className="primary" onClick={onAdd}>
          新增账号
        </button>
      </div>
      <table>
        <thead>
          <tr>
            <th>账号名</th>
            <th>默认桶</th>
            <th>AK</th>
            <th>SK</th>
            <th>root 路径</th>
            <th>用量</th>
            <th>状态</th>
            <th>备注</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {accounts.map((a) => {
            const revealed = a.sk.includes('****')
            return (
              <tr key={a.name}>
                <td>{a.name}</td>
                <td className={a.bucketExists ? 'path' : 'muted'}>
                  {a.bucketExists ? a.bucket : '未建桶'}
                </td>
                <td>
                  <code>{a.ak}</code> <CopyButton value={a.ak} />
                </td>
                <td>
                  <code>{a.sk}</code>{' '}
                  {revealed ? (
                    <button type="button" className="link" onClick={() => onReveal(a.name)}>
                      显示
                    </button>
                  ) : (
                    <CopyButton value={a.sk} />
                  )}
                </td>
                <td className="path">{a.root}</td>
                <td>{formatBytes(a.usageBytes)}</td>
                <td>
                  <span className={`status ${a.status}`}>{statusLabel[a.status]}</span>
                </td>
                <td>{a.note}</td>
                <td className="ops">
                  <button type="button" className="link" onClick={() => onRotate(a.name)}>
                    轮换 SK
                  </button>
                  <button type="button" className="link" onClick={() => onToggle(a)}>
                    {a.disabled ? '启用' : '停用'}
                  </button>
                  <button type="button" className="link" onClick={() => onEdit(a)}>
                    编辑
                  </button>
                  <button type="button" className="link danger" onClick={() => onDelete(a.name)}>
                    删除
                  </button>
                </td>
              </tr>
            )
          })}
          {accounts.length === 0 ? (
            <tr>
              <td colSpan={9} className="muted">
                暂无账号
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </main>
  )
}

// ---- 主应用 ----

interface SecretState {
  title: string
  name: string
  ak?: string
  sk: string
  endpoint?: string
  region?: string
  pathStyle?: boolean
}

export default function App() {
  const mode: AuthMode = window.__OBJBOX_AUTH_MODE__ === 'sso' ? 'sso' : 'builtin'
  const [, navigate] = useHashRoute()
  const [authed, setAuthed] = useState(false)
  const [user, setUser] = useState('')
  const [overview, setOverview] = useState<Overview | null>(null)
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [loginError, setLoginError] = useState('')
  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Account | null>(null)
  const [secret, setSecret] = useState<SecretState | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [ov, list] = await Promise.all([api.overview(), api.accounts()])
      setOverview(ov)
      setAccounts(list.accounts)
      setAuthed(true)
      setUser(mode === 'sso' ? 'sso 用户' : 'admin')
      navigate('/accounts')
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        setAuthed(false)
        if (mode === 'builtin') {
          navigate('/login')
        }
      } else {
        setError(e instanceof Error ? e.message : '加载失败')
      }
    } finally {
      setLoading(false)
    }
  }, [mode, navigate])

  useEffect(() => {
    void load()
  }, [load])

  const refresh = useCallback(async () => {
    const [ov, list] = await Promise.all([api.overview(), api.accounts()])
    setOverview(ov)
    setAccounts(list.accounts)
  }, [])

  const onLogin = async (password: string) => {
    setLoginError('')
    try {
      await api.login(password)
      await load()
    } catch (e) {
      setLoginError(e instanceof Error ? e.message : '登录失败')
    }
  }

  const onLogout = async () => {
    try {
      await api.logout()
    } catch {
      // 退出失败也回到登录页
    }
    setAuthed(false)
    setAccounts([])
    setOverview(null)
    navigate('/login')
  }

  const onReveal = async (name: string) => {
    try {
      const r = await api.reveal(name)
      const found = r.accounts.find((a) => a.name === name)
      if (found) {
        setAccounts((prev) => prev.map((a) => (a.name === name ? { ...a, sk: found.sk } : a)))
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取失败')
    }
  }

  const onRotate = async (name: string) => {
    if (!window.confirm(`确定轮换账号 ${name} 的 SK？旧 SK 将立即失效。`)) {
      return
    }
    try {
      const r = await api.rotate(name)
      setSecret({ title: '新的 SK（仅显示一次）', name, sk: r.sk })
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '轮换失败')
    }
  }

  const onToggle = async (a: Account) => {
    try {
      await api.setDisabled(a.name, !a.disabled)
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '操作失败')
    }
  }

  const onDelete = async (name: string) => {
    if (!window.confirm(`确定删除账号 ${name}？账号表条目会被移除，数据目录保留。`)) {
      return
    }
    try {
      await api.remove(name)
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败')
    }
  }

  if (loading && !authed) {
    return <div className="center muted">加载中…</div>
  }

  if (!authed) {
    if (mode === 'sso') {
      return <ForbiddenView />
    }
    return <LoginView error={loginError} onSubmit={(p) => void onLogin(p)} />
  }

  return (
    <div className="app">
      <TopBar mode={mode} user={user} overview={overview} onLogout={() => void onLogout()} />
      {error ? <div className="banner error">{error}</div> : null}
      <AccountsView
        accounts={accounts}
        onAdd={() => setAddOpen(true)}
        onReveal={(n) => void onReveal(n)}
        onRotate={(n) => void onRotate(n)}
        onToggle={(a) => void onToggle(a)}
        onEdit={(a) => setEditTarget(a)}
        onDelete={(n) => void onDelete(n)}
      />
      {addOpen ? (
        <AddAccountModal
          onClose={() => setAddOpen(false)}
          onCreated={(cred) => {
            setAddOpen(false)
            setSecret({
              title: '账号已创建（密钥仅显示一次）',
              name: cred.name,
              ak: cred.ak,
              sk: cred.sk,
              endpoint: cred.endpoint,
              region: cred.region,
              pathStyle: cred.pathStyle,
            })
            void refresh()
          }}
        />
      ) : null}
      {editTarget ? (
        <EditAccountModal
          account={editTarget}
          onClose={() => setEditTarget(null)}
          onSaved={() => {
            setEditTarget(null)
            void refresh()
          }}
        />
      ) : null}
      {secret ? (
        <SecretModal
          title={secret.title}
          name={secret.name}
          ak={secret.ak}
          sk={secret.sk}
          endpoint={secret.endpoint}
          region={secret.region}
          pathStyle={secret.pathStyle}
          onClose={() => setSecret(null)}
        />
      ) : null}
    </div>
  )
}
