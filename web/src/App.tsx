import { useCallback, useEffect, useState, useSyncExternalStore, type ReactNode } from 'react'
import {
  Alert,
  Button,
  Card,
  Checkbox,
  Descriptions,
  Drawer,
  Form,
  Input,
  Layout,
  Menu,
  Modal,
  Popconfirm,
  Segmented,
  Space,
  Table,
  Tag,
  Typography,
  theme as antdTheme,
} from 'antd'
import type { DescriptionsProps, TableProps } from 'antd'
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
import { setThemeMode, useThemeMode, type ThemeMode } from './theme'

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

// useNarrow：窄屏判定，用于把顶栏操作收进抽屉。
function useNarrow(): boolean {
  const subscribe = useCallback((callback: () => void) => {
    const mql = window.matchMedia('(max-width: 767px)')
    mql.addEventListener('change', callback)
    return () => mql.removeEventListener('change', callback)
  }, [])
  const getSnapshot = useCallback(() => window.matchMedia('(max-width: 767px)').matches, [])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
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

const statusColor: Record<Account['status'], string> = {
  enabled: 'success',
  disabled: 'error',
  readonly: 'warning',
}

// Field：弹层里「标签在上、控件在下」的简洁排版。
function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="objbox-field">
      <Typography.Text>{label}</Typography.Text>
      {children}
    </div>
  )
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
    <Button type="link" size="small" disabled={disabled} onClick={() => void copy()}>
      {done ? '已复制' : label}
    </Button>
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
    <Space size={4} wrap>
      <Typography.Text code className="objbox-mono">
        {display}
      </Typography.Text>
      <Button type="link" size="small" onClick={onToggle}>
        {shown ? '隐藏' : '显示'}
      </Button>
      <CopyButton value={secret ?? ''} disabled={!shown || !secret} />
    </Space>
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

  const columns: TableProps<BucketInfo>['columns'] = [
    {
      title: '桶名',
      dataIndex: 'name',
      width: 240,
      render: (_value, b) => (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {b.name}
          </Typography.Text>
          <CopyButton value={b.name} />
        </Space>
      ),
    },
    { title: '对象数', dataIndex: 'objects', width: 100 },
    { title: '占用', dataIndex: 'bytes', width: 120, render: (value: number) => formatBytes(value) },
    {
      title: '默认',
      dataIndex: 'isDefault',
      width: 90,
      render: (value: boolean) => (value ? '默认' : ''),
    },
    {
      title: '操作',
      key: 'ops',
      fixed: 'right',
      width: 100,
      render: (_value, b) => (
        <Popconfirm
          title={`确定删除桶 ${b.name}？仅空桶可删，桶内还有对象时需先清空。`}
          onConfirm={() => void remove(b)}
        >
          <Button type="link" size="small" danger>
            删除
          </Button>
        </Popconfirm>
      ),
    },
  ]

  return (
    <div className="objbox-buckets">
      <div className="objbox-section-head">
        <Typography.Text strong>桶</Typography.Text>
        <Button type="link" size="small" onClick={() => void load()}>
          刷新
        </Button>
      </div>
      <Table
        rowKey="name"
        size="small"
        columns={columns}
        dataSource={buckets}
        loading={loading}
        pagination={false}
        scroll={{ x: 650 }}
        locale={{ emptyText: '暂无桶' }}
      />

      <Space.Compact style={{ width: '100%' }}>
        <Input
          value={newName}
          placeholder="新桶名（3-63 位）"
          onChange={(e) => setNewName(e.target.value)}
        />
        <Button type="primary" loading={busy} onClick={() => void create()}>
          新建桶
        </Button>
      </Space.Compact>

      <Space align="end" wrap>
        <Field label="默认桶">
          <Input value={editBucket} onChange={(e) => setEditBucket(e.target.value)} />
        </Field>
        <Checkbox checked={editAuto} onChange={(e) => setEditAuto(e.target.checked)}>
          自动创建同名桶
        </Checkbox>
        <Button loading={saveBusy} onClick={() => void save()}>
          保存
        </Button>
      </Space>
      {error ? <Alert type="error" showIcon message={error} /> : null}
    </div>
  )
}

// ---- 设置弹窗 ----

// SettingsModal 是全站设置（当前仅跨域白名单），入口在标题栏「设置」。
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
    <Modal
      title="设置"
      open
      onCancel={onClose}
      width={520}
      footer={[
        <Button key="close" onClick={onClose}>
          关闭
        </Button>,
        <Button key="save" type="primary" loading={saving} onClick={() => void save()}>
          保存
        </Button>,
      ]}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Title level={5} style={{ margin: 0 }}>
          跨域白名单（CORS）
        </Typography.Title>
        <Typography.Text type="secondary">
          允许这些来源的网页在浏览器里直接读写本对象存储（预签名直传/直下）。留空表示关闭跨域。请只填你自己的前端域名。
        </Typography.Text>
        <Field label="来源（每行一个，也支持逗号分隔）">
          <Input.TextArea
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
        </Field>
        {error || validationError ? (
          <Alert type="error" showIcon message={error || validationError} />
        ) : null}
        <Typography.Text type="secondary">
          当前生效：
          {loading ? '加载中…' : effective.length > 0 ? effective.join('、') : '未开启跨域'}
        </Typography.Text>
        {saved ? <Alert type="success" showIcon message="已保存并即时生效" /> : null}
      </Space>
    </Modal>
  )
}

// ---- 登录 / 未认证 ----

function ThemeSwitcher() {
  const mode = useThemeMode()
  return (
    <Segmented
      size="small"
      value={mode}
      onChange={(value) => setThemeMode(value as ThemeMode)}
      options={[
        { label: '自动', value: 'auto' },
        { label: '亮色', value: 'light' },
        { label: '暗色', value: 'dark' },
      ]}
    />
  )
}

function LoginView({
  error,
  onSubmit,
}: {
  error: string
  onSubmit: (password: string) => void
}) {
  const [form] = Form.useForm<{ password: string }>()
  return (
    <div className="objbox-center">
      <div className="objbox-topright">
        <ThemeSwitcher />
      </div>
      <Card className="objbox-login-card">
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          objbox 管理
        </Typography.Title>
        <Typography.Text type="secondary">请输入管理员口令</Typography.Text>
        <Form form={form} layout="vertical" onFinish={(values) => onSubmit(values.password)}>
          <Form.Item name="password">
            <Input.Password autoFocus placeholder="管理员口令" />
          </Form.Item>
          {error ? <Alert type="error" showIcon message={error} /> : null}
          <Button type="primary" htmlType="submit" block style={{ marginTop: 12 }}>
            登录
          </Button>
        </Form>
      </Card>
    </div>
  )
}

function ForbiddenView() {
  return (
    <div className="objbox-center">
      <div className="objbox-topright">
        <ThemeSwitcher />
      </div>
      <Card className="objbox-login-card">
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          未认证
        </Typography.Title>
        <Typography.Text type="secondary">
          当前为 SSO 模式，请通过网关携带 X-Auth-User 访问。
        </Typography.Text>
        <Alert type="error" showIcon message="401 Unauthorized" style={{ marginTop: 12 }} />
      </Card>
    </div>
  )
}

// ---- 顶部栏 ----

function TopBar({
  mode,
  user,
  overview,
  isNarrow,
  onLogout,
  onOpenSettings,
  onOpenMenu,
}: {
  mode: AuthMode
  user: string
  overview: Overview | null
  isNarrow: boolean
  onLogout: () => void
  onOpenSettings: () => void
  onOpenMenu: () => void
}) {
  const { token } = antdTheme.useToken()
  const light = { color: token.colorTextLightSolid }
  const stats = (
    <Space size={16} style={{ opacity: 0.85 }}>
      <Tag color="blue">{mode === 'sso' ? 'SSO' : 'builtin'}</Tag>
      <span>账号 {overview ? overview.accounts : 0}</span>
      <span>总用量 {formatBytes(overview ? overview.totalUsageBytes : 0)}</span>
    </Space>
  )
  return (
    <Layout.Header className="objbox-header" style={light}>
      <span className="objbox-brand">objbox 管理</span>
      {!isNarrow ? stats : null}
      <div className="objbox-header-right" style={light}>
        {isNarrow ? (
          <Button type="link" style={light} onClick={onOpenMenu}>
            菜单
          </Button>
        ) : (
          <>
            <ThemeSwitcher />
            <Button type="link" style={light} onClick={onOpenSettings}>
              设置
            </Button>
            {mode === 'sso' ? (
              <span style={{ opacity: 0.85 }}>当前用户：{user || '未知'}</span>
            ) : (
              <Button type="link" style={light} onClick={onLogout}>
                退出
              </Button>
            )}
          </>
        )}
      </div>
    </Layout.Header>
  )
}

// 窄屏抽屉：把统计、主题三态、设置与退出收进来，确保切换控件始终可达。
function NarrowMenu({
  open,
  mode,
  user,
  overview,
  onClose,
  onOpenSettings,
  onLogout,
}: {
  open: boolean
  mode: AuthMode
  user: string
  overview: Overview | null
  onClose: () => void
  onOpenSettings: () => void
  onLogout: () => void
}) {
  return (
    <Drawer placement="right" open={open} onClose={onClose} width={280}>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <ThemeSwitcher />
        <Space direction="vertical" size={4}>
          <Tag color="blue">{mode === 'sso' ? 'SSO' : 'builtin'}</Tag>
          <span>账号 {overview ? overview.accounts : 0}</span>
          <span>总用量 {formatBytes(overview ? overview.totalUsageBytes : 0)}</span>
        </Space>
        <Button
          onClick={() => {
            onClose()
            onOpenSettings()
          }}
        >
          设置
        </Button>
        {mode === 'sso' ? (
          <Typography.Text>当前用户：{user || '未知'}</Typography.Text>
        ) : (
          <Button onClick={onLogout}>退出</Button>
        )}
      </Space>
    </Drawer>
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
    <Modal
      title="新增账号"
      open
      onCancel={onClose}
      onOk={() => void submit()}
      okText="创建"
      cancelText="取消"
      confirmLoading={busy}
      okButtonProps={{ disabled: !valid }}
      width={480}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Field label="账号名（[a-z0-9-]，1-32 位）">
          <Input value={name} autoFocus onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="备注">
          <Input value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
        <Checkbox checked={autoCreate} onChange={(e) => setAutoCreate(e.target.checked)}>
          自动创建同名桶
        </Checkbox>
        <Field label="默认桶名（留空 = 账号名）">
          <Input
            value={bucket}
            disabled={!autoCreate}
            placeholder={name || '账号名'}
            onChange={(e) => setBucket(e.target.value)}
          />
        </Field>
        {error ? <Alert type="error" showIcon message={error} /> : null}
      </Space>
    </Modal>
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
    <Modal
      title={`编辑账号 ${account.name}`}
      open
      onCancel={onClose}
      onOk={() => void submit()}
      okText="保存"
      cancelText="取消"
      confirmLoading={busy}
      width={480}
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Field label="备注">
          <Input value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
        <Field label="配额（字节，0 表示不限）">
          <Input value={quota} onChange={(e) => setQuota(e.target.value)} />
        </Field>
        {error ? <Alert type="error" showIcon message={error} /> : null}
      </Space>
    </Modal>
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

  const items: DescriptionsProps['items'] = [
    {
      key: 'endpoint',
      label: 'Endpoint',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {endpoint}
          </Typography.Text>
          <CopyButton value={endpoint} />
        </Space>
      ),
    },
    {
      key: 'bucket',
      label: 'Bucket',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {defaultBucket}
          </Typography.Text>
          <CopyButton value={defaultBucket} />
        </Space>
      ),
    },
    {
      key: 'ak',
      label: 'Access Key',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {account.ak}
          </Typography.Text>
          <CopyButton value={account.ak} />
        </Space>
      ),
    },
    {
      key: 'sk',
      label: 'Secret Key',
      children: (
        <SecretCell
          masked={account.sk}
          secret={secrets[account.name]}
          shown={secretShown}
          onToggle={() => onToggleSecret(account.name)}
        />
      ),
    },
    {
      key: 'region',
      label: 'Region',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {region}
          </Typography.Text>
          <CopyButton value={region} />
        </Space>
      ),
    },
    {
      key: 'addressing',
      label: 'Addressing',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {addressing}
          </Typography.Text>
          <CopyButton value={addressing} />
          <Typography.Text type="secondary">客户端里需选 Path-style</Typography.Text>
        </Space>
      ),
    },
  ]

  return (
    <Modal
      title={`账号详情 ${account.name}`}
      open
      onCancel={onClose}
      width={680}
      footer={
        <Button type="primary" onClick={onClose}>
          关闭
        </Button>
      }
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Descriptions column={1} size="small" bordered items={items} />

        <div>
          <div className="objbox-section-head">
            <Typography.Text strong>示例配置</Typography.Text>
            <CopyButton label="复制示例" value={example} />
          </div>
          <pre className="objbox-copyblock">{example}</pre>
        </div>

        <Typography.Text type="secondary">
          桶不存在会自动创建，Bucket 那栏随便填或填账号名即可。
        </Typography.Text>

        <BucketPanel account={account} onAccountChanged={onAccountChanged} />
      </Space>
    </Modal>
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
    <Modal
      title={`桶管理 ${account.name}`}
      open
      onCancel={onClose}
      width={680}
      footer={
        <Button type="primary" onClick={onClose}>
          关闭
        </Button>
      }
    >
      <BucketPanel account={account} onAccountChanged={onAccountChanged} />
    </Modal>
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
  const items: DescriptionsProps['items'] = [
    {
      key: 'name',
      label: '账号',
      children: (
        <Space size={4} wrap>
          {name}
          <CopyButton value={name} />
        </Space>
      ),
    },
  ]
  if (ak) {
    items.push({
      key: 'ak',
      label: 'AK',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {ak}
          </Typography.Text>
          <CopyButton value={ak} />
        </Space>
      ),
    })
  }
  items.push({
    key: 'sk',
    label: 'SK',
    children: (
      <Space size={4} wrap>
        <Typography.Text code className="objbox-mono">
          {sk}
        </Typography.Text>
        <CopyButton value={sk} />
      </Space>
    ),
  })
  if (endpoint) {
    items.push({
      key: 'endpoint',
      label: 'Endpoint',
      children: (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {endpoint}
          </Typography.Text>
          <CopyButton value={endpoint} />
        </Space>
      ),
    })
  }
  if (region) {
    items.push({
      key: 'region',
      label: 'Region',
      children: (
        <Typography.Text code className="objbox-mono">
          {region}
        </Typography.Text>
      ),
    })
  }
  if (pathStyle !== undefined) {
    items.push({
      key: 'pathstyle',
      label: 'PathStyle',
      children: pathStyle ? 'true' : 'false',
    })
  }

  return (
    <Modal
      title={title}
      open
      onCancel={onClose}
      width={560}
      footer={
        <Button type="primary" onClick={onClose}>
          我已保存
        </Button>
      }
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Alert type="warning" showIcon message="关闭后不再显示，请立即保存。" />
        <Descriptions column={1} size="small" bordered items={items} />
        {ak && endpoint ? (
          <CopyButton
            label="复制连接信息"
            value={`Endpoint: ${endpoint}\nRegion: ${region ?? ''}\nPathStyle: ${pathStyle}\nAK: ${ak}\nSK: ${sk}`}
          />
        ) : null}
      </Space>
    </Modal>
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
  const columns: TableProps<Account>['columns'] = [
    { title: '账号名', dataIndex: 'name', width: 120 },
    {
      title: '默认桶',
      dataIndex: 'bucket',
      width: 140,
      render: (_value, a) =>
        a.bucketExists ? (
          <Typography.Text className="objbox-mono">{a.bucket}</Typography.Text>
        ) : (
          <Typography.Text type="secondary">未建桶</Typography.Text>
        ),
    },
    {
      title: 'AK',
      dataIndex: 'ak',
      width: 200,
      render: (_value, a) => (
        <Space size={4} wrap>
          <Typography.Text code className="objbox-mono">
            {a.ak}
          </Typography.Text>
          <CopyButton value={a.ak} />
        </Space>
      ),
    },
    {
      title: 'SK',
      dataIndex: 'sk',
      width: 220,
      render: (_value, a) => (
        <SecretCell
          masked={a.sk}
          secret={secrets[a.name]}
          shown={!!shown[a.name]}
          onToggle={() => onToggleSecret(a.name)}
        />
      ),
    },
    {
      title: 'root 路径',
      dataIndex: 'root',
      width: 220,
      render: (value: string) => <Typography.Text className="objbox-mono">{value}</Typography.Text>,
    },
    {
      title: '用量',
      dataIndex: 'usageBytes',
      width: 100,
      render: (value: number) => formatBytes(value),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (_value, a) => <Tag color={statusColor[a.status]}>{statusLabel[a.status]}</Tag>,
    },
    { title: '备注', dataIndex: 'note', width: 140 },
    {
      title: '操作',
      key: 'ops',
      fixed: 'right',
      width: 300,
      render: (_value, a) => (
        <Space size={0} wrap>
          <Button type="link" size="small" onClick={() => onDetail(a)}>
            详情
          </Button>
          <Button type="link" size="small" onClick={() => onBuckets(a)}>
            桶管理
          </Button>
          <Popconfirm
            title={`确定轮换账号 ${a.name} 的 SK？旧 SK 将立即失效。`}
            onConfirm={() => onRotate(a.name)}
          >
            <Button type="link" size="small">
              轮换 SK
            </Button>
          </Popconfirm>
          <Button type="link" size="small" onClick={() => onToggle(a)}>
            {a.disabled ? '启用' : '停用'}
          </Button>
          <Button type="link" size="small" onClick={() => onEdit(a)}>
            编辑
          </Button>
          <Popconfirm
            title={`确定删除账号 ${a.name}？账号表条目会被移除，数据目录保留。`}
            onConfirm={() => onDelete(a.name)}
          >
            <Button type="link" size="small" danger>
              删除
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <div className="objbox-section-head" style={{ marginBottom: 0 }}>
        <Typography.Title level={4} style={{ margin: 0 }}>
          账号列表
        </Typography.Title>
        <Button type="primary" onClick={onAdd}>
          新增账号
        </Button>
      </div>
      <Table
        rowKey="name"
        columns={columns}
        dataSource={accounts}
        pagination={false}
        scroll={{ x: 1530 }}
        sticky={{ offsetHeader: 64 }}
        locale={{ emptyText: '暂无账号' }}
      />
    </Space>
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
  const isNarrow = useNarrow()
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
  const [menuOpen, setMenuOpen] = useState(false)
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
    try {
      await api.remove(name)
      await refresh()
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败')
    }
  }

  if (loading && !authed) {
    return (
      <div className="objbox-center">
        <Typography.Text type="secondary">加载中…</Typography.Text>
      </div>
    )
  }

  if (!authed) {
    if (mode === 'sso') {
      return <ForbiddenView />
    }
    return <LoginView error={loginError} onSubmit={(p) => void onLogin(p)} />
  }

  return (
    <Layout className="objbox-layout">
      <TopBar
        mode={mode}
        user={user}
        overview={overview}
        isNarrow={isNarrow}
        onLogout={() => void onLogout()}
        onOpenSettings={() => setSettingsOpen(true)}
        onOpenMenu={() => setMenuOpen(true)}
      />
      <Layout>
        {!isNarrow ? (
          <Layout.Sider theme="light" width={180} breakpoint="lg">
            <Menu
              mode="inline"
              selectedKeys={['/accounts']}
              onClick={() => navigate('/accounts')}
              items={[{ key: '/accounts', label: '账号' }]}
            />
          </Layout.Sider>
        ) : null}
        <Layout.Content className="objbox-content">
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            {error ? <Alert type="error" showIcon message={error} /> : null}
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
          </Space>
        </Layout.Content>
      </Layout>

      <NarrowMenu
        open={menuOpen}
        mode={mode}
        user={user}
        overview={overview}
        onClose={() => setMenuOpen(false)}
        onOpenSettings={() => setSettingsOpen(true)}
        onLogout={() => void onLogout()}
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
    </Layout>
  )
}
