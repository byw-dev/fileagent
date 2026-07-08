import { App } from 'antd'
import type { ReactNode } from 'react'

/**
 * Danger-action confirmation hook (交互定则 2) for simple confirms (吊销 / 停用 …):
 * a red primary button + an explicit consequence description. For the
 * "type the name to confirm" variant (4f) use `DangerConfirmModal` instead.
 */

export interface DangerConfirmOptions {
  /** Title, e.g. `吊销采集器：sensor-01`. */
  title: ReactNode
  /** Consequence description shown in the body. */
  content?: ReactNode
  /** Primary (danger) button label, default 确认. */
  okText?: string
  /** Confirm handler; may be async — the modal spins until it settles. */
  onOk: () => void | Promise<void>
}

/**
 * useDangerConfirm — returns a function that opens a themed danger confirm
 * (red primary button, Chinese 取消/确认). Uses `App.useApp().modal` so it runs
 * under the app's ConfigProvider/theme context.
 */
export function useDangerConfirm() {
  const { modal } = App.useApp()
  return (options: DangerConfirmOptions) => {
    modal.confirm({
      title: options.title,
      content: options.content,
      okText: options.okText ?? '确认',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: options.onOk,
    })
  }
}
