import { Drawer, Space, Button } from 'antd'
import type { ReactNode } from 'react'

interface FormDrawerProps {
  /** Whether the drawer is open. */
  open: boolean
  /** Drawer title. */
  title: ReactNode
  /** Close handler (cancel button + mask + Esc). */
  onClose: () => void
  /** Submit handler for the default footer's primary button. */
  onSubmit?: () => void
  /** Whether the submit action is in flight (spinner + disables cancel). */
  loading?: boolean
  /** Primary button label (default 保存). */
  submitText?: string
  /** Hide the built-in footer (provide your own via children). */
  hideFooter?: boolean
  /** Drawer width (default 480, the single fixed drawer size · §1.4). */
  width?: number
  /** Form body. */
  children: ReactNode
}

/**
 * FormDrawer — the site-wide 480px right drawer that carries create/edit forms
 * (交互定则 1: 新建/编辑一律用抽屉，不整页跳转、不弹窗承载表单).
 *
 * Provides a standard footer (取消 + one primary button). Pages own the form
 * inside `children` and drive submission via `onSubmit`/`loading`.
 */
function FormDrawer({
  open,
  title,
  onClose,
  onSubmit,
  loading = false,
  submitText = '保存',
  hideFooter = false,
  width = 480,
  children,
}: FormDrawerProps) {
  return (
    <Drawer
      open={open}
      title={title}
      onClose={onClose}
      width={width}
      maskClosable={!loading}
      destroyOnHidden
      footer={
        hideFooter ? undefined : (
          <div style={{ textAlign: 'right' }}>
            <Space>
              <Button onClick={onClose} disabled={loading}>
                取消
              </Button>
              <Button type="primary" loading={loading} onClick={onSubmit}>
                {submitText}
              </Button>
            </Space>
          </div>
        )
      }
    >
      {children}
    </Drawer>
  )
}

export default FormDrawer
