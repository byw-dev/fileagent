import { useState } from 'react'
import { Button, Space, Typography, Modal, message, Tag } from 'antd'
import { PlusOutlined, DeleteOutlined } from '@ant-design/icons'
import { ProTable } from '@ant-design/pro-components'
import type { ProColumns, ActionType } from '@ant-design/pro-components'
import { useNavigate } from 'react-router-dom'
import { useRef } from 'react'
import { listFileTypes, deleteFileType } from '../../services/file-types'
import type { FileType } from '../../services/file-types'

const { Title } = Typography

/**
 * File types list page — shows all file types with create and delete actions.
 */
function FileTypesPage() {
  const navigate = useNavigate()
  const actionRef = useRef<ActionType | undefined>(undefined)
  const [deleting, setDeleting] = useState<string | null>(null)

  const handleDelete = (ft: FileType) => {
    Modal.confirm({
      title: `删除文件类型：${ft.name}`,
      content: '确认删除该文件类型？',
      okType: 'danger',
      onOk: async () => {
        setDeleting(ft.id)
        try {
          await deleteFileType(ft.id)
          message.success('删除成功')
          actionRef.current?.reload()
        } catch {
          message.error('删除失败')
        } finally {
          setDeleting(null)
        }
      },
    })
  }

  const columns: ProColumns<FileType>[] = [
    {
      title: '名称',
      dataIndex: 'name',
      key: 'name',
      render: (_, ft) => (
        <a onClick={() => navigate(`/file-types/${ft.id}`)}>{ft.name}</a>
      ),
    },
    {
      title: '描述',
      dataIndex: 'description',
      key: 'description',
      ellipsis: true,
      render: (_, ft) => ft.description ?? <Tag color="default">无</Tag>,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      key: 'created_at',
      width: 170,
      render: (_, ft) => new Date(ft.created_at).toLocaleString('zh-CN'),
    },
    {
      title: '操作',
      key: 'action',
      width: 100,
      render: (_, ft) => (
        <Button
          size="small"
          danger
          icon={<DeleteOutlined />}
          loading={deleting === ft.id}
          onClick={() => handleDelete(ft)}
        >
          删除
        </Button>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Title level={3} style={{ margin: 0 }}>文件类型</Title>
        <Button
          type="primary"
          icon={<PlusOutlined />}
          onClick={() => navigate('/file-types/create')}
        >
          新建类型
        </Button>
      </Space>

      <ProTable<FileType>
        actionRef={actionRef}
        columns={columns}
        rowKey="id"
        search={false}
        pagination={{ pageSize: 20 }}
        request={async () => {
          try {
            const data = await listFileTypes()
            return { data, success: true, total: data.length }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
      />
    </div>
  )
}

export default FileTypesPage
