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
  assert.match(appSource, /<button type="button" className="primary"[^>]*>\s*保存\s*</)
})
