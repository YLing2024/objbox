// 前端轻量测试：不引入任何新依赖，使用 Node 内置的 node:test 与原生 TS 类型剥离运行。
//   cd web && npm test
// 覆盖：跨域来源解析/校验规则；以及「设置」入口改造的源码结构回归
// （本仓库没有 DOM 测试环境，点击/输入等交互行为以人工自测为准，见交付报告）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

import { firstInvalid, parseOrigins, validateOrigin } from '../src/cors.ts'

const here = dirname(fileURLToPath(import.meta.url))
const appSource = readFileSync(join(here, '..', 'src', 'App.tsx'), 'utf8')

test('parseOrigins：每行一个来源，也支持逗号分隔，并保留行号', () => {
  const got = parseOrigins(
    'https://app.example.com, http://127.0.0.1:5173\n\nhttps://b.example.com',
  )
  assert.deepEqual(got, [
    { origin: 'https://app.example.com', line: 1 },
    { origin: 'http://127.0.0.1:5173', line: 1 },
    { origin: 'https://b.example.com', line: 3 },
  ])
})

test('validateOrigin：接受 scheme://host[:port] 与末尾斜杠', () => {
  assert.equal(validateOrigin('https://app.example.com'), '')
  assert.equal(validateOrigin('http://127.0.0.1:5173'), '')
  assert.equal(validateOrigin('https://app.example.com/'), '')
})

test('validateOrigin：拒绝非法来源', () => {
  assert.notEqual(validateOrigin('not-a-url'), '')
  assert.notEqual(validateOrigin('ftp://app.example.com'), '')
  assert.notEqual(validateOrigin('https://app.example.com/path'), '')
  assert.notEqual(validateOrigin('https://app.example.com?q=1'), '')
  assert.notEqual(validateOrigin('https://user:pass@app.example.com'), '')
})

test('firstInvalid：指出第几行与原文，并给出格式要求', () => {
  const msg = firstInvalid('https://app.example.com\nnot-a-url')
  assert.match(msg, /^第 2 行不合法：「not-a-url」。/)
  assert.match(msg, /scheme:\/\/host/)
})

test('firstInvalid：全部合法时返回空串', () => {
  assert.equal(firstInvalid('https://app.example.com\nhttp://127.0.0.1:5173'), '')
})

test('标题栏新增「设置」入口，正文不再渲染跨域白名单一节', () => {
  assert.match(appSource, />\s*设置\s*</)
  assert.match(appSource, /onOpenSettings/)
  assert.doesNotMatch(appSource, /<CORSPanel\b/)
})

test('设置弹窗包含小节标题、原文说明与保存/关闭按钮', () => {
  assert.match(appSource, /function SettingsModal\b/)
  assert.match(appSource, /跨域白名单（CORS）/)
  assert.match(appSource, /留空表示关闭跨域/)
  assert.match(appSource, /已保存并即时生效/)
  assert.match(appSource, /<Button[^>]*type="primary"[\s\S]*?>\s*保存\s*</)
})

// —— 第 1 轮 antd 迁移与三态主题的结构回归 ——

const here2 = dirname(fileURLToPath(import.meta.url))
const themeSource = readFileSync(join(here2, '..', 'src', 'theme.ts'), 'utf8')
const indexSource = readFileSync(join(here2, '..', 'index.html'), 'utf8')
const mainSource = readFileSync(join(here2, '..', 'src', 'main.tsx'), 'utf8')

test('theme.ts：localStorage 键 theme_mode，导出 useThemeMode/setThemeMode 并监听系统变化', () => {
  assert.match(themeSource, /theme_mode/)
  assert.match(themeSource, /export function useThemeMode\b/)
  assert.match(themeSource, /export function setThemeMode\b/)
  assert.match(themeSource, /prefers-color-scheme: dark/)
  assert.match(themeSource, /data-theme/)
  assert.match(themeSource, /color-scheme/)
})

test('index.html：首屏绘制前内联脚本应用主题（防闪白）', () => {
  assert.match(indexSource, /theme_mode/)
  assert.match(indexSource, /data-theme/)
  assert.match(indexSource, /color-scheme/)
  assert.match(indexSource, /__OBJBOX_AUTH_MODE_VALUE__/)
})

test('main.tsx：包 ConfigProvider(locale+algorithm) 与 antd App，无自定义 token 映射', () => {
  assert.match(mainSource, /ConfigProvider/)
  assert.match(mainSource, /antdTheme\.darkAlgorithm/)
  assert.match(mainSource, /antdTheme\.defaultAlgorithm/)
  assert.match(mainSource, /AntdApp|App as AntdApp/)
  // theme 里只允许 algorithm / locale，不得出现 token/components 自定义映射
  assert.doesNotMatch(mainSource, /\btoken\s*:/)
  assert.doesNotMatch(mainSource, /\bcomponents\s*:/)
})

test('App.tsx：操作列 narrow 取消固定、放宽列宽；每张 Table 走 max-content 横滑', () => {
  // 桌面固定右列保持不变；窄屏改为随整表横滑，避免固定列宽大于视口时错位。
  assert.match(appSource, /fixed:\s*isNarrow\s*\?\s*undefined\s*:\s*'right'/)
  assert.match(appSource, /width:\s*isNarrow\s*\?\s*360\s*:\s*300/)
  assert.match(appSource, /sticky=\{\{\s*offsetHeader:\s*64\s*\}\}/)

  const tables = appSource.match(/<Table\b/g) ?? []
  const maxContent = appSource.match(/scroll=\{\{\s*x:\s*'max-content'\s*\}\}/g) ?? []
  assert.equal(tables.length, 2, '全仓应有两张 Table')
  assert.equal(maxContent.length, tables.length, '每张 Table 都要带 scroll x max-content')
  // 不许再有写死的横向像素宽（会让窄屏装上不下又无横滑的原因之一）。
  assert.doesNotMatch(appSource, /scroll=\{\{\s*x:\s*\d/)
})

test('App.tsx：窄屏断点 991.98px（≥992px 一律桌面分支）', () => {
  assert.match(appSource, /NARROW_QUERY/)
  assert.match(appSource, /991\.98px/)
})

test('App.tsx：窄屏导航不隐藏，改由抽屉承载（与桌面一致的账号入口）', () => {
  assert.match(appSource, /onNavigate/)
  assert.match(appSource, /function NarrowMenu\b/)
  // 抽屉里的 Menu 与桌面 Sider 用同一批 items。
  const menus = appSource.match(/selectedKeys=\{\['\/accounts'\]\}/g) ?? []
  assert.ok(menus.length >= 2, '桌面侧栏与窄屏抽屉都要有账号导航')
})

// —— 响应式样式回归（jsdom 量不到像素，改断言媒体查询/阈值，见交付报告说明）——

const stylesSource = readFileSync(join(here, '..', 'src', 'styles.css'), 'utf8')
const mediaStart = stylesSource.indexOf('@media (max-width: 991.98px)')
const mediaBlock = mediaStart === -1 ? '' : stylesSource.slice(mediaStart)

test('styles.css：响应式规则只在窄屏/粗指针媒体查询内，且同一条条件同时覆盖两者', () => {
  assert.match(stylesSource, /@media\s*\(max-width:\s*991\.98px\),\s*\(pointer:\s*coarse\)/)
})

test('styles.css：触摸目标达标（主操作 44、图标 32、输入 44、勾选 44、间距 8）', () => {
  assert.match(mediaBlock, /\.ant-btn\s*\{[^}]*min-height:\s*44px/s)
  assert.match(mediaBlock, /\.ant-btn\.ant-btn-icon-only[\s\S]*?min-width:\s*32px/)
  assert.match(mediaBlock, /\.ant-input[\s\S]*?min-height:\s*44px/)
  assert.match(mediaBlock, /\.ant-checkbox-wrapper\s*\{[^}]*min-height:\s*44px/s)
  assert.match(
    mediaBlock,
    /\.ant-space:not\(\.ant-space-compact\)\s*\{[^}]*column-gap:\s*8px\s*!important/s,
  )
})

test('styles.css：可见文本 ≥12px（antd code 默认 85%，窄屏抬回 12px）', () => {
  assert.match(mediaBlock, /\.objbox-mono code[\s\S]*?font-size:\s*12px/)
  const sizes = [...stylesSource.matchAll(/font-size:\s*(\d+(?:\.\d+)?)px/g)].map((m) =>
    Number(m[1]),
  )
  assert.ok(sizes.length > 0)
  for (const size of sizes) {
    assert.ok(size >= 12, `自定义字号 ${size}px 低于 12px`)
  }
})

test('styles.css：不给容器加 overflow:hidden（保持表体横滑可用）', () => {
  assert.doesNotMatch(stylesSource, /overflow(-x|-y)?:\s*hidden/)
})

test('docs/COMPONENTS.md：可复用组件已登记且文件头有用途说明', () => {
  const doc = readFileSync(join(here, '..', '..', 'docs', 'COMPONENTS.md'), 'utf8')
  for (const name of ['Field', 'CopyButton', 'SecretCell']) {
    assert.match(doc, new RegExp(`\\b${name}\\b`))
  }
  assert.match(appSource, /\/\/ Field：/)
  assert.match(appSource, /function CopyButton\b/)
  assert.match(appSource, /function SecretCell\b/)
})


test('App.tsx：主题三态 Segmented（自动/亮色/暗色）', () => {
  assert.match(appSource, /自动/)
  assert.match(appSource, /亮色/)
  assert.match(appSource, /暗色/)
  assert.match(appSource, /<Segmented\b/)
})
