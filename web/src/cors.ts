// 跨域白名单（CORS）来源的解析与校验。
//
// 规则与后端 internal/settings.Normalize 保持一致：来源必须是
// scheme://host 或 scheme://host:port，scheme 只支持 http/https。
// 这里只用于管理页的就地提示，最终以后端校验为准。

export interface OriginToken {
  origin: string
  line: number
}

// 解析文本域内容：每行一个来源，同一行也允许逗号分隔，保留 1 起算的行号。
export function parseOrigins(text: string): OriginToken[] {
  const out: OriginToken[] = []
  text.split('\n').forEach((line, i) => {
    line.split(',').forEach((token) => {
      const t = token.trim()
      if (t) {
        out.push({ origin: t, line: i + 1 })
      }
    })
  })
  return out
}

// 校验单个来源，合法返回空串，否则返回可读的失败原因。
export function validateOrigin(raw: string): string {
  const s = raw.trim()
  if (!s) {
    return '不能为空'
  }
  const hint = '应为 scheme://host 或 scheme://host:port，scheme 限 http/https'
  let u: URL
  try {
    u = new URL(s.replace(/\/$/, ''))
  } catch {
    return hint
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    return hint
  }
  if (!u.hostname) {
    return '缺少主机名'
  }
  if (u.username || u.password) {
    return '不得包含用户名或密码'
  }
  if (u.pathname !== '' && u.pathname !== '/') {
    return '只能填 scheme://host[:port]，不得带路径'
  }
  if (u.search || u.hash) {
    return '不得带查询或片段'
  }
  if (u.port && (Number(u.port) < 1 || Number(u.port) > 65535)) {
    return '端口号非法'
  }
  return ''
}

// 返回第一处非法项的提示（含行号与原文），全部合法时返回空串。
export function firstInvalid(text: string): string {
  for (const item of parseOrigins(text)) {
    const problem = validateOrigin(item.origin)
    if (problem) {
      return `第 ${item.line} 行不合法：「${item.origin}」。${problem}`
    }
  }
  return ''
}
