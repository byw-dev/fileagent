import { useState } from 'react'
import { Modal, Input, Typography } from 'antd'
import type { ReactNode } from 'react'

/**
 * DangerConfirmModal — controlled danger confirmation (交互定则 2) with an
 * optional name-to-confirm gate (4f). Renders a red primary button and an
 * explicit consequence description.
 *
 * For simple confirms without a keyword gate, prefer the `useDangerConfirm`
 * hook (src/hooks/useDangerConfirm.ts).
 */

interface DangerConfirmModalProps {
  /** Whether the modal is open. */
  open: boolean
  /** Title, e.g. `删除文件类型`. */
  title: ReactNode
  /** Consequence description (e.g. 「该类型下有 N 个文件」). */
  content?: ReactNode
  /** Cancel handler. */
  onCancel: () => void
  /** Confirm handler. */
  onConfirm: () => void
  /** In-flight state. */
  loading?: boolean
  /** Danger button label, default 删除. */
  okText?: string
  /** When set, the user must type this exact string to enable the danger button. */
  confirmKeyword?: string
}

/**
 * DangerConfirmModal — controlled danger confirmation with an optional
 * name-to-confirm gate. Renders a red primary button.
 */
export function DangerConfirmModal({
  open,
  title,
  content,
  onCancel,
  onConfirm,
  loading = false,
  okText = '删除',
  confirmKeyword,
}: DangerConfirmModalProps) {
  const [typed, setTyped] = useState('')
  const gated = Boolean(confirmKeyword)
  const okEnabled = !gated || typed === confirmKeyword

  return (
    <Modal
      open={open}
      title={title}
      onCancel={onCancel}
      onOk={onConfirm}
      okText={okText}
      cancelText="取消"
      confirmLoading={loading}
      okButtonProps={{ danger: true, disabled: !okEnabled }}
      // While the danger action is in flight, lock every dismiss path (cancel
      // button / mask / Esc / close icon) so the modal stays open until the
      // request settles — mirrors FormDrawer, avoids "looks cancelled but the
      // delete still completed" (交互定则 2).
      cancelButtonProps={{ disabled: loading }}
      maskClosable={!loading}
      keyboard={!loading}
      closable={!loading}
      afterClose={() => setTyped('')}
      destroyOnHidden
    >
      {content}
      {gated && (
        <div style={{ marginTop: 12 }}>
          <Typography.Text type="secondary">
            请输入 <Typography.Text strong>{confirmKeyword}</Typography.Text> 以确认：
          </Typography.Text>
          <Input
            style={{ marginTop: 8 }}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            placeholder={confirmKeyword}
            autoFocus
          />
        </div>
      )}
    </Modal>
  )
}

export default DangerConfirmModal
