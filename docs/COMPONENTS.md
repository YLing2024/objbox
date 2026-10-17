# 前端组件登记表

本文件登记 `web/src` 下的可复用组件。新增组件必须同一笔提交登记；删除组件同步删除条目。
设计语言：antd 默认设计语言（颜色/圆角/字号一律走 antd 自带，不在组件里新造）。
页面级视图（`LoginView`/`ForbiddenView`/`TopBar`/`NarrowMenu`/`*Modal`/`AccountsView`）只在本应用内使用一次，作为视图不登记；其中复用的构件登记如下。

| 组件 | 用途 | 关键 props / 事件 | 最小示例 | 注意事项 |
|---|---|---|---|---|
| `Field` | 弹层里「标签在上、控件在下」的表单行 | `label: ReactNode`（必填）、`children: ReactNode`（必填） | `<Field label="备注"><Input value={note} onChange={...} /></Field>` | 纯布局，不接管校验；窄屏由父级 `Space wrap` 自适应到单列 |
| `CopyButton` | 复制文本到剪贴板的行内按钮 | `value: string`、`label?: string`（默认 `复制`）、`disabled?: boolean`（默认 `false`） | `<CopyButton value={a.ak} />` | 剪贴板不可用时静默失败；按钮为次要操作，窄屏下由全局媒体查询保证触摸目标尺寸 |
| `SecretCell` | SK 掩码/明文切换单元格，列表与详情窗口共用 | `masked: string`、`secret?: string`、`shown: boolean`、`onToggle: () => void` | `<SecretCell masked={a.sk} secret={secrets[a.name]} shown={!!shown[a.name]} onToggle={() => onToggleSecret(a.name)} />` | 点击一次切换常显、再点隐藏，不依赖 hover（触摸与鼠标一致）；掩码时复制按钮禁用；不改变脱敏逻辑 |
| `BucketPanel` | 桶管理面板，账号详情窗口与「桶管理」窗口共用 | `account: Account`、`onAccountChanged: (a: Account) => void` | `<BucketPanel account={account} onAccountChanged={onAccountChanged} />` | 内嵌 Table 带 `scroll={{ x: 'max-content' }}`；新建/保存会写后端，改动后经 `onAccountChanged` 回传 |

## 可访问性要点

- `CopyButton`/`SecretCell` 用 antd `Button`，键盘可达（Enter/Space 触发），点击态有焦点样式。
- 窄屏与 `pointer: coarse` 下，`styles.css` 的媒体查询把可交互控件抬到主操作 ≥44px、图标 ≥32×32、勾选框命中区 ≥44px，并保证互相间距 ≥8px；桌面（≥992px）不写规则，观感逐像素不变。
- 表格在任意宽度都通过 `scroll={{ x: 'max-content' }}` 提供横向滚动，操作列窄屏取消固定以避免固定列宽大于视口时错位。
