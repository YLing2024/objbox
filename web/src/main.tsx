import React from 'react'
import { createRoot } from 'react-dom/client'
import { App as AntdApp, ConfigProvider, theme as antdTheme } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import App from './App'
import { useDarkMode } from './theme'
import './styles.css'

// Root 只负责把「解析后的明暗」接进 antd：theme 里**只允许** algorithm 与 locale，
// 绝不做任何自定义 token 映射（不要把旧配色/直角塞进 theme）。
function Root() {
  const dark = useDarkMode()
  return (
    <ConfigProvider
      locale={zhCN}
      theme={{
        algorithm: dark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
      }}
    >
      <AntdApp>
        <App />
      </AntdApp>
    </ConfigProvider>
  )
}

const root = document.getElementById('root')
if (root) {
  createRoot(root).render(
    <React.StrictMode>
      <Root />
    </React.StrictMode>,
  )
}
