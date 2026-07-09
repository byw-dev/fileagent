import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { ConfigProvider, App as AntdApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import App from './App.tsx'
import themeConfig from './theme'
import './index.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ConfigProvider locale={zhCN} theme={themeConfig}>
      {/* AntdApp provides the context required for App.useApp() hooks (modal, message,
          notification). Without it, the static Modal.confirm() / message.xxx() APIs may
          silently fail in React 18 StrictMode. */}
      <AntdApp>
        <App />
      </AntdApp>
    </ConfigProvider>
  </StrictMode>,
)
