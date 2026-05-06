import { useState } from 'react'
import { Button, Form, Input, Typography, Space, message, Card } from 'antd'
import { useNavigate } from 'react-router-dom'
import { createFileType } from '../../services/file-types'

const { Title } = Typography
const { TextArea } = Input

/**
 * Create file type page — form with name and optional description.
 */
function FileTypeCreatePage() {
  const navigate = useNavigate()
  const [form] = Form.useForm()
  const [submitting, setSubmitting] = useState(false)

  const handleSubmit = async (values: { name: string; description?: string }) => {
    setSubmitting(true)
    try {
      await createFileType(values)
      message.success('文件类型创建成功')
      navigate('/file-types')
    } catch {
      message.error('创建失败，请重试')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button onClick={() => navigate('/file-types')}>← 返回列表</Button>
        <Title level={3} style={{ margin: 0 }}>新建文件类型</Title>
      </Space>

      <Card style={{ maxWidth: 600 }}>
        <Form
          form={form}
          layout="vertical"
          onFinish={handleSubmit}
        >
          <Form.Item
            label="名称"
            name="name"
            rules={[{ required: true, message: '请输入文件类型名称' }]}
          >
            <Input placeholder="例如：日志文件" maxLength={64} />
          </Form.Item>

          <Form.Item label="描述" name="description">
            <TextArea
              placeholder="可选描述"
              maxLength={256}
              rows={3}
              showCount
            />
          </Form.Item>

          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit" loading={submitting}>
                创建
              </Button>
              <Button onClick={() => navigate('/file-types')}>取消</Button>
            </Space>
          </Form.Item>
        </Form>
      </Card>
    </div>
  )
}

export default FileTypeCreatePage
