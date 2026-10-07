import { useCallback, useEffect, useState } from 'react'
import {
  ApiError,
  api,
  type Account,
  type AccountList,
  type AuthMode,
  type BucketInfo,
  type OneTimeCredential,
  type Overview,
} from './api'
import { firstInvalid, parseOrigins } from './cors'

// ---- 小工具 ----

function useHashRoute(): [string, (r: string) => void] {
  const read = () => window.location.hash.replace(/^#/, '') || '/'
  const [route, setRoute] = useState(read)
  useEffect(() => {
    const onChange = () => setRoute(read())
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  // 注意：navigate 必须保持**稳定引用**。它被 load()（useCallback 依赖）与 useEffect([load]) 间接引用，
  // 每次渲染新建函数会让 load 每次换身份 → effect 每次重跑 → 首屏无限重刷（表现为登录框一闪一闪 + 接口风暴）。
  const navigate = useCallback((r: string) => {
    if (read() !== r) {
      window.location.hash = r
    }
  }, [])
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

function CopyButton({
  value,
  label = '复制',
  disabled = false,
}: {
  value: string
  label?: string
  disabled?: boolean
}) {
  const [done, setDone] = useState(false)
  const copy = async () => {
    if (disabled) {
      return
    }
    try {
      await navigator.clipboard.writeText(value)
      setDone(true)
      window.setTimeout(() => setDone(false), 1500)
    } catch {
      // 剪贴板不可用时静默失败
    }
  }
  return (
    <button type="button" className="link" disabled={disabled} onClick={copy}>
      {done ? '已复制' : label}
    </button>
  )
}

// SK 显隐：点击一次切换为常显，再点一次隐藏（不依赖 hover / 按住 / 鼠标按键，触摸与鼠标一致）。
// 列表与详情窗口共用本组件，掩码时复制按钮禁用、常显时可复制。
// 手工验证：桌面点击「显示」一次 → 保持明文；手机触摸点击一次 → 同样保持明文，不会立即回弹。
function SecretCell({
  masked,
  secret,
  shown,
  onToggle,
}: {
  masked: string
  secret?: string
  shown: boolean
  onToggle: () => void
}) {
  const display = shown && secret ? secret : masked
  return (
    <span className="secret">
      <code>{display}</code>{' '}
      <button type="button" className="link" onClick={onToggle}>
        {shown ? '隐藏' : '显示'}
      </button>{' '}
      <CopyButton value={secret ?? ''} disabled={!shown || !secret} />
    </span>
  )
}

const bucketNameRe = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/

// validateBucketName 与后端 ValidateBucket 规则一致，仅用于就地提示。
function validateBucketName(name: string): string {
  if (name.length < 3 || name.length > 63) {
    return '桶名长度需在 3-63 之间'
  }
  if (name.includes('..')) {
    return '桶名不能包含 ..'
  }
  if (!bucketNameRe.test(name)) {
    return '桶名只允许小写字母、数字、点与连字符，且首尾不能是点或连字符'
  }
  return ''
}

// BucketPanel 是桶管理面板：账号详情窗口与「桶管理」窗口共用同一组件。
function BucketPanel({
  account,
  onAccountChanged,
}: {
  account: Account
  onAccountChanged: (a: Account) => void
}) {
  const [buckets, setBuckets] = useState<BucketInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [newName, setNewName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [editBucket, setEditBucket] = useState(account.bucket)
  const [editAuto, setEditAuto] = useState(account.autoCreateBucket)
  const [saveBusy, setSaveBusy] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await api.listBuckets(account.name)
      setBuckets(r.buckets)
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取桶列表失败')
    } finally {
      setLoading(false)
    }
  }, [account.name])

  useEffect(() => {
    void load()
  }, [load])

  useEffect(() => {
    setEditBucket(account.bucket)
    setEditAuto(account.autoCreateBucket)
  }, [account.bucket, account.autoCreateBucket])

  const create = async () => {
    const name = newName.trim()
    const problem = validateBucketName(name)
    if (problem) {
      setError(problem)
      return
    }
    setBusy(true)
    setError('')
    try {
      await api.createBucket(account.name, name)
      setNewName('')
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : '建桶失败')
    } finally {
      setBusy(false)
    }
  }

  const remove = async (b: BucketInfo) => {
    if (!window.confirm(`确定删除桶 ${b.name}？仅空桶可删，桶内还有对象时需先清空。`)) {
      return
    }
    setError('')
    try {
      await api.deleteBucket(account.name, b.name)
      await load()
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除桶失败')
    }
  }

  const save = async () => {
    const name = editBucket.trim()
    const problem = validateBucketName(name)
    if (problem) {
      setError(problem)
      return
    }
    setSaveBusy(true)
    setError('')
    try {
      const updated = await api.updateBucket(account.name, {
        bucket: name,
        autoCreateBucket: editAuto,
      })
      onAccountChanged(updated)
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaveBusy(false)
    }
  }

  return (
    <div className="buckets">
      <div className="section-head">
        <span className="section-title">桶</span>
        <button type="button" className="link" onClick={() => void load()}>
          刷新
        </button>
      </div>
      <table className="buckets-table">
        <thead>
          <tr>
            <th>桶名</th>
            <th>对象数</th>
            <th>占用</th>
            <th>默认</th>
            <th>操作</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.name}>
              <td>
                <code>{b.name}</code> <CopyButton value={b.name} />
              </td>
              <td>{b.objects}</td>
              <td>{formatBytes(b.bytes)}</td>
              <td>{b.isDefault ? '默认' : ''}</td>
              <td>
                <button type="button" className="link danger" onClick={() => void remove(b)}>
                  删除
                </button>
              </td>
            </tr>
          ))}
          {loading ? (
            <tr>
              <td colSpan={5} className="muted">
                加载中…
              </td>
            </tr>
          ) : buckets.length === 0 ? (
            <tr>
              <td colSpan={5} className="muted">
                暂无桶
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>

      <div className="bucket-create">
        <input
          value={newName}
          placeholder="新桶名（3-63 位）"
          onChange={(e) => setNewName(e.target.value)}
        />
        <button type="button" className="primary" disabled={busy} onClick={() => void create()}>
          新建桶
        </button>
      </div>

      <div className="bucket-default">
        <label>
          默认桶
          <input value={editBucket} onChange={(e) => setEditBucket(e.target.value)} />
        </label>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={editAuto}
            onChange={(e) => setEditAuto(e.target.checked)}
          />
          自动创建同名桶
        </label>
        <button type="button" disabled={saveBusy} onClick={() => void save()}>
          保存
        </button>
      </div>
      {error ? <p className="error">{error}</p> : null}
    </div>
  )
}

// ---- 设置弹窗 ----

// SettingsModal 是全站设置（当前仅跨域白名单），入口在标题栏「设置」。
// 复用页面既有的 overlay + card modal + actions 结构；遮罩或「关闭」关闭，
// 未保存的编辑随组件卸载丢弃，重新打开会重新拉取已保存值。
function SettingsModal({ onClose }: { onClose: () => void }) {
  const [value, setValue] = useState('')
  const [effective, setEffective] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const r = await api.settings()
      setValue(r.corsOrigins.join('\n'))
      setEffective(r.corsOrigins)
    } catch (e) {
      setError(e instanceof Error ? e.message : '读取跨域设置失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const validationError = firstInvalid(value)

  const save = async () => {
    if (validationError) {
      setError(validationError)
      return
    }
    setSaving(true)
    setError('')
    try {
      const origins = parseOrigins(value).map((item) => item.origin)
      const r = await api.updateSettings(origins)
      setEffective(r.corsOrigins)
      setValue(r.corsOrigins.join('\n'))
      setSaved(true)
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="card modal" onClick={(e) => e.stopPropagation()}>
        <h2>设置</h2>
        <div className="section-head">
          <span className="section-title">跨域白名单（CORS）</span>
        </div>
        <p className="muted">
          允许这些来源的网页在浏览器里直接读写本对象存储（预签名直传/直下）。留空表示关闭跨域。请只填你自己的前端域名。
        </p>
        <label className="cors-editor">
          来源（每行一个，也支持逗号分隔）
          <textarea
            rows={4}
            value={value}
            spellCheck={false}
            placeholder="https://app.example.com"
            onChange={(e) => {
              setValue(e.target.value)
              setSaved(false)
              setError('')
            }}
          />
        </label>
        {error || validationError ? <p className="error">{error || validationError}</p> : null}
        <p className="muted">
          当前生效：
          {loading ? '加载中…' : effective.length > 0 ? effective.join('、') : '未开启跨域'}
        </p>
        {saved ? <p className="ok">已保存并即时生效</p> : null}
        <div className="actions">
          <button type="button" className="primary" disabled={saving} onClick={() => void save()}>
            保存
          </button>
          <button type="button" onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </div>
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
  onOpenSettings,
}: {
  mode: AuthMode
  user: string
  overview: Overview | null
  onLogout: () => void
  onOpenSettings: () => void
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
        <button type="button" className="link" onClick={onOpenSettings}>
          设置
        </button>
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

// ---- 账号详情窗口 ----

function AccountDetailModal({
  account,
  endpoint,
  region,
  pathStyle,
  secrets,
  shown,
  onToggleSecret,
  onAccountChanged,
  onClose,
}: {
  account: Account
  endpoint: string
  region: string
  pathStyle: boolean
  secrets: Record<string, string>
  shown: Record<string, boolean>
  onToggleSecret: (name: string) => void
  onAccountChanged: (a: Account) => void
  onClose: () => void
}) {
  const secretShown = !!shown[account.name]
  const sk = secretShown && secrets[account.name] ? secrets[account.name] : account.sk
  const defaultBucket = account.bucket || account.name
  const addressing = pathStyle ? 'Path-style' : 'Virtual-hosted-style'
  const example = [
    `rclone config create objbox s3 provider Other env_auth false \\`,
    `  access_key_id ${account.ak} secret_access_key ${sk} \\`,
    `  endpoint ${endpoint} region ${region}`,
    '',
    `通用 S3 客户端：`,
    `Endpoint: ${endpoint}`,
    `AK: ${account.ak}`,
    `SK: ${sk}`,
    `Bucket: ${defaultBucket}`,
    `Region: ${region}`,
    `寻址: ${addressing}`,
  ].join('\n')

  return (
    <div className="overlay">
      <div className="card modal wide">
        <h2>账号详情 {account.name}</h2>
        <dl className="kv">
          <dt>Endpoint</dt>
          <dd>
            <code>{endpoint}</code> <CopyButton value={endpoint} />
          </dd>
          <dt>Bucket</dt>
          <dd>
            <code>{defaultBucket}</code> <CopyButton value={defaultBucket} />
          </dd>
          <dt>Access Key</dt>
          <dd>
            <code>{account.ak}</code> <CopyButton value={account.ak} />
          </dd>
          <dt>Secret Key</dt>
          <dd>
            <SecretCell
              masked={account.sk}
              secret={secrets[account.name]}
              shown={secretShown}
              onToggle={() => onToggleSecret(account.name)}
            />
          </dd>
          <dt>Region</dt>
          <dd>
            <code>{region}</code> <CopyButton value={region} />
          </dd>
          <dt>Addressing</dt>
          <dd>
            <code>{addressing}</code> <CopyButton value={addressing} />{' '}
            <span className="muted">客户端里需选 Path-style</span>
          </dd>
        </dl>

        <div className="example">
          <div className="section-head">
            <span className="section-title">示例配置</span>
            <CopyButton label="复制示例" value={example} />
          </div>
          <pre className="copyblock">{example}</pre>
        </div>

        <p className="muted">桶不存在会自动创建，Bucket 那栏随便填或填账号名即可。</p>

        <BucketPanel account={account} onAccountChanged={onAccountChanged} />

        <div className="actions">
          <button type="button" className="primary" onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </div>
  )
}

// ---- 桶管理窗口 ----

function BucketManageModal({
  account,
  onAccountChanged,
  onClose,
}: {
  account: Account
  onAccountChanged: (a: Account) => void
  onClose: () => void
}) {
  return (
    <div className="overlay">
      <div className="card modal wide">
        <h2>桶管理 {account.name}</h2>
        <BucketPanel account={account} onAccountChanged={onAccountChanged} />
        <div className="actions">
          <button type="button" className="primary" onClick={onClose}>
            关闭
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
  secrets,
  shown,
  onToggleSecret,
  onAdd,
  onRotate,
  onToggle,
  onEdit,
  onDelete,
  onDetail,
  onBuckets,
}: {
  accounts: Account[]
  secrets: Record<string, string>
  shown: Record<string, boolean>
  onToggleSecret: (name: string) => void
  onAdd: () => void
  onRotate: (name: string) => void
  onToggle: (a: Account) => void
  onEdit: (a: Account) => void
  onDelete: (name: string) => void
  onDetail: (a: Account) => void
  onBuckets: (a: Account) => void
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
          {accounts.map((a) => (
            <tr key={a.name}>
              <td>{a.name}</td>
              <td className={a.bucketExists ? 'path' : 'muted'}>
                {a.bucketExists ? a.bucket : '未建桶'}
              </td>
              <td>
                <code>{a.ak}</code> <CopyButton value={a.ak} />
              </td>
              <td>
                <SecretCell
                  masked={a.sk}
                  secret={secrets[a.name]}
                  shown={!!shown[a.name]}
                  onToggle={() => onToggleSecret(a.name)}
                />
              </td>
              <td className="path">{a.root}</td>
              <td>{formatBytes(a.usageBytes)}</td>
              <td>
                <span className={`status ${a.status}`}>{statusLabel[a.status]}</span>
              </td>
              <td>{a.note}</td>
              <td className="ops">
                <button type="button" className="link" onClick={() => onDetail(a)}>
                  详情
                </button>
                <button type="button" className="link" onClick={() => onBuckets(a)}>
                  桶管理
                </button>
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
          ))}
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
  const [endpoint, setEndpoint] = useState('')
  const [region, setRegion] = useState('us-east-1')
  const [pathStyle, setPathStyle] = useState(true)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [loginError, setLoginError] = useState('')
  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Account | null>(null)
  const [detail, setDetail] = useState<Account | null>(null)
  const [bucketTarget, setBucketTarget] = useState<Account | null>(null)
  const [secret, setSecret] = useState<SecretState | null>(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [secrets, setSecrets] = useState<Record<string, string>>({})
  const [shown, setShown] = useState<Record<string, boolean>>({})

  const applyList = useCallback((list: AccountList) => {
    setAccounts(list.accounts)
    setEndpoint(list.endpoint && list.endpoint !== '' ? list.endpoint : window.location.origin)
    if (list.region) {
      setRegion(list.region)
    }
    if (list.pathStyle !== undefined) {
      setPathStyle(list.pathStyle)
    }
    return list.accounts
  }, [])

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const [ov, list] = await Promise.all([api.overview(), api.accounts()])
      setOverview(ov)
      applyList(list)
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
  }, [mode, navigate, applyList])

  useEffect(() => {
    void load()
  }, [load])

  const refresh = useCallback(async () => {
    const [ov, list] = await Promise.all([api.overview(), api.accounts()])
    setOverview(ov)
    return applyList(list)
  }, [applyList])

  const applyAccount = useCallback((updated: Account) => {
    setAccounts((prev) => prev.map((a) => (a.name === updated.name ? updated : a)))
    setDetail((prev) => (prev && prev.name === updated.name ? updated : prev))
    setBucketTarget((prev) => (prev && prev.name === updated.name ? updated : prev))
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

  const onToggleSecret = useCallback(
    async (name: string) => {
      if (shown[name]) {
        setShown((prev) => ({ ...prev, [name]: false }))
        return
      }
      if (!secrets[name]) {
        try {
          const r = await api.reveal(name)
          const found = r.accounts.find((a) => a.name === name)
          if (!found) {
            setError('读取 SK 失败')
            return
          }
          setSecrets((prev) => ({ ...prev, [name]: found.sk }))
        } catch (e) {
          setError(e instanceof Error ? e.message : '读取 SK 失败')
          return
        }
      }
      setShown((prev) => ({ ...prev, [name]: true }))
    },
    [shown, secrets],
  )

  const closeDetail = () => {
    if (detail) {
      setShown((prev) => ({ ...prev, [detail.name]: false }))
    }
    setDetail(null)
  }

  const onRotate = async (name: string) => {
    if (!window.confirm(`确定轮换账号 ${name} 的 SK？旧 SK 将立即失效。`)) {
      return
    }
    try {
      const r = await api.rotate(name)
      setSecrets((prev) => ({ ...prev, [name]: r.sk }))
      setShown((prev) => ({ ...prev, [name]: false }))
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
      <TopBar
        mode={mode}
        user={user}
        overview={overview}
        onLogout={() => void onLogout()}
        onOpenSettings={() => setSettingsOpen(true)}
      />
      {error ? <div className="banner error">{error}</div> : null}
      <AccountsView
        accounts={accounts}
        secrets={secrets}
        shown={shown}
        onToggleSecret={(n) => void onToggleSecret(n)}
        onAdd={() => setAddOpen(true)}
        onRotate={(n) => void onRotate(n)}
        onToggle={(a) => void onToggle(a)}
        onEdit={(a) => setEditTarget(a)}
        onDelete={(n) => void onDelete(n)}
        onDetail={(a) => setDetail(a)}
        onBuckets={(a) => setBucketTarget(a)}
      />
      {addOpen ? (
        <AddAccountModal
          onClose={() => setAddOpen(false)}
          onCreated={(cred) => {
            setAddOpen(false)
            setSecrets((prev) => ({ ...prev, [cred.name]: cred.sk }))
            setShown((prev) => ({ ...prev, [cred.name]: true }))
            void (async () => {
              try {
                const list = await refresh()
                const found = list.find((a) => a.name === cred.name)
                if (found) {
                  setDetail(found)
                }
              } catch (e) {
                setError(e instanceof Error ? e.message : '加载失败')
              }
            })()
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
      {detail ? (
        <AccountDetailModal
          account={detail}
          endpoint={endpoint}
          region={region}
          pathStyle={pathStyle}
          secrets={secrets}
          shown={shown}
          onToggleSecret={(n) => void onToggleSecret(n)}
          onAccountChanged={applyAccount}
          onClose={closeDetail}
        />
      ) : null}
      {bucketTarget ? (
        <BucketManageModal
          account={bucketTarget}
          onAccountChanged={applyAccount}
          onClose={() => setBucketTarget(null)}
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
      {settingsOpen ? <SettingsModal onClose={() => setSettingsOpen(false)} /> : null}
    </div>
  )
}
