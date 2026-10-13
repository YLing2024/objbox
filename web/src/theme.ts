// 主题三态（auto | light | dark）。
//
// - localStorage 键固定为 theme_mode，取值 auto|light|dark，缺省 auto。
// - auto 跟随系统 prefers-color-scheme，并监听其 change 事件实时切换。
// - DOM 侧只做两件事：给 <html> 打 data-theme=light|dark，并同步 <meta name="color-scheme">。
//   首屏绘制前的应用由 index.html 内联脚本完成（防闪白），这里只做运行期兜底与响应。
//
// 本模块不涉及任何自定义色值 / token 映射：明暗只通过 antd 的 algorithm 切换（见 main.tsx）。

import { useSyncExternalStore } from 'react'

export type ThemeMode = 'auto' | 'light' | 'dark'

const STORAGE_KEY = 'theme_mode'
const DARK_QUERY = '(prefers-color-scheme: dark)'

function preferences(): MediaQueryList | null {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') {
    return null
  }
  return window.matchMedia(DARK_QUERY)
}

function prefersDark(): boolean {
  const mql = preferences()
  return mql ? mql.matches : false
}

function readMode(): ThemeMode {
  try {
    const value = window.localStorage.getItem(STORAGE_KEY)
    if (value === 'light' || value === 'dark' || value === 'auto') {
      return value
    }
  } catch {
    // 隐私模式等 localStorage 不可用时退回 auto
  }
  return 'auto'
}

// apply 只写 data-theme 与 meta[name=color-scheme]，不写入任何具体颜色。
function apply(mode: ThemeMode): void {
  if (typeof document === 'undefined') {
    return
  }
  const dark = mode === 'dark' || (mode === 'auto' && prefersDark())
  const resolved = dark ? 'dark' : 'light'
  const root = document.documentElement
  root.setAttribute('data-theme', resolved)
  // color-scheme 交给浏览器决定 UA 默认底色（深色下不再是白），避免首屏白闪。
  root.style.colorScheme = resolved
  let meta = document.querySelector('meta[name="color-scheme"]')
  if (!meta) {
    meta = document.createElement('meta')
    meta.setAttribute('name', 'color-scheme')
    document.head.appendChild(meta)
  }
  meta.setAttribute('content', resolved)
}

let mode: ThemeMode = typeof window === 'undefined' ? 'auto' : readMode()
let isDark = mode === 'dark' || (mode === 'auto' && prefersDark())

const listeners = new Set<() => void>()
function emit(): void {
  for (const listener of listeners) {
    listener()
  }
}

function sync(): void {
  isDark = mode === 'dark' || (mode === 'auto' && prefersDark())
  apply(mode)
  emit()
}

const initial = preferences()
if (initial) {
  const onChange = () => {
    // 只有 auto 才跟随系统；显式 light/dark 时忽略系统变化。
    if (mode === 'auto') {
      sync()
    }
  }
  if (typeof initial.addEventListener === 'function') {
    initial.addEventListener('change', onChange)
  } else if (typeof initial.addListener === 'function') {
    initial.addListener(onChange)
  }
}

// 运行期兜底：首屏已由 index.html 内联脚本应用，这里保证 meta 与 React 状态一致。
if (typeof document !== 'undefined') {
  apply(mode)
}

function subscribe(callback: () => void): () => void {
  listeners.add(callback)
  return () => {
    listeners.delete(callback)
  }
}

export function getThemeMode(): ThemeMode {
  return mode
}

export function setThemeMode(next: ThemeMode): void {
  mode = next
  try {
    window.localStorage.setItem(STORAGE_KEY, next)
  } catch {
    // 写入失败只影响持久化，不影响当前会话生效
  }
  sync()
}

export function useThemeMode(): ThemeMode {
  return useSyncExternalStore(subscribe, getThemeMode, getThemeMode)
}

export function useDarkMode(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => isDark,
    () => isDark,
  )
}
