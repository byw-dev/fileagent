import { useState, useEffect } from 'react'
import {
  App,
  Typography,
  Button,
  Space,
  Alert,
  Card,
} from 'antd'
import { StepsForm, ProFormText, ProFormSelect, ProFormSwitch } from '@ant-design/pro-components'
import { useParams, useNavigate } from 'react-router-dom'
import { createRule } from '../../services/agents'
import type { CollectionMode } from '../../services/agents'
import apiClient from '../../services/api'
import {
  renderPathPreview,
  validatePathTemplate,
  PATH_TEMPLATE_VARIABLES,
} from '../../utils/pathTemplate'

const { Title, Text } = Typography

/** Parse a cron expression and return a human-readable description */
function describeCron(expr: string): string {
  if (!expr || expr.trim() === '') return ''
  const parts = expr.trim().split(/\s+/)
  if (parts.length !== 5) return '无效的 cron 表达式（需要5个字段）'
  const [min, hour, dom, mon, dow] = parts
  if (min === '*' && hour === '*' && dom === '*' && mon === '*' && dow === '*') {
    return '每分钟执行一次'
  }
  if (dom === '*' && mon === '*' && dow === '*') {
    if (min === '0') return `每天 ${hour.padStart(2, '0')}:00 执行`
    if (hour === '*') return `每小时第 ${min} 分钟执行`
    return `每天 ${hour.padStart(2, '0')}:${min.padStart(2, '0')} 执行`
  }
  return `cron: ${expr}`
}

interface Bucket {
  id: string
  name: string
}

/** Flat form values merged by StepsForm.onFinish across all 3 steps. */
interface RuleFormValues {
  name: string
  mode: CollectionMode
  bucket_id: string
  base_path: string
  path_pattern: string
  cron_expr?: string
  run_once_on_start?: boolean
  recursive?: boolean
  append_mode?: string
  enabled?: boolean
  dest_path_template: string
}

/**
 * Agent rule creation form — 3-step ProForm.
 * Step 1: Basic config (name, mode, target bucket).
 * Step 2: Source path config (Watch vs Scheduled fields differ).
 * Step 3: Upload path template with live preview.
 */
function AgentRuleFormPage() {
  const { id: agentId } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { message } = App.useApp()

  const [mode, setMode] = useState<CollectionMode>('WATCH')
  const [cronExpr, setCronExpr] = useState('')
  const [pathTemplate, setPathTemplate] = useState('/{year}/{month}/{day}/{filename}')
  const [pathError, setPathError] = useState<string | null>(null)
  const [buckets, setBuckets] = useState<Bucket[]>([])
  const [submitting, setSubmitting] = useState(false)

  useEffect(() => {
    apiClient
      .get<{ items: Bucket[] }>('/api/v1/buckets')
      .then((res) => setBuckets(res.data.items ?? []))
      .catch(() => {})
  }, [])

  const handlePathTemplateChange = (value: string) => {
    setPathTemplate(value)
    setPathError(validatePathTemplate(value))
  }

  /**
   * Called by StepsForm.onFinish with the flat merged values from all steps.
   * StepsForm merges step values via Object.assign, so the result is a flat
   * object — NOT nested under step names.
   */
  const handleFinish = async (values: RuleFormValues): Promise<boolean> => {
    if (!agentId) {
      message.error('采集器 ID 缺失，请刷新页面后重试')
      return false
    }
    setSubmitting(true)
    try {
      await createRule(agentId, {
        name: values.name,
        mode: values.mode,
        bucket_id: values.bucket_id,
        base_path: values.base_path,
        path_pattern: values.path_pattern,
        cron_expr: values.mode === 'SCHEDULED' ? (values.cron_expr ?? null) : null,
        run_once_on_start: values.mode === 'SCHEDULED' ? (values.run_once_on_start ?? false) : false,
        dest_path_template: values.dest_path_template,
        recursive: values.recursive ?? false,
        append_mode: values.append_mode ?? 'overwrite',
        enabled: values.enabled ?? true,
      })
      message.success('规则创建成功')
      navigate(`/agents/${agentId}`, { state: { tab: 'rules' } })
      return true
    } catch {
      message.error('规则创建失败，请稍后重试')
      return false
    } finally {
      setSubmitting(false)
    }
  }

  const bucketOptions = buckets.map((b) => ({ label: b.name, value: b.id }))

  return (
    <div style={{ maxWidth: 760, margin: '0 auto' }}>
      <Space style={{ marginBottom: 24 }}>
        <Button onClick={() => navigate(`/agents/${agentId}`)}>← 返回</Button>
        <Title level={4} style={{ margin: 0 }}>新建采集规则</Title>
      </Space>

      {/*
        NOTE: StepsForm.submitter.render is the ONLY place to customise step
        buttons.  Any `submitter` prop placed on a StepForm child is silently
        overridden to `false` by the parent StepsForm.
      */}
      <StepsForm<RuleFormValues>
        submitter={{
          render: (props) => {
            if (props.step === 0) {
              return (
                <Button type="primary" onClick={() => props.onSubmit?.()}>
                  下一步
                </Button>
              )
            }
            if (props.step === 1) {
              return (
                <Space>
                  <Button onClick={() => props.onPre?.()}>上一步</Button>
                  <Button type="primary" onClick={() => props.onSubmit?.()}>
                    下一步
                  </Button>
                </Space>
              )
            }
            // Last step (step 2)
            return (
              <Space>
                <Button onClick={() => props.onPre?.()}>上一步</Button>
                <Button type="primary" loading={submitting} onClick={() => props.onSubmit?.()}>
                  创建规则
                </Button>
              </Space>
            )
          },
        }}
        onFinish={async (values) => {
          // StepsForm merges all step values into a single flat object before
          // calling onFinish — there is no nesting by step name.
          return await handleFinish(values)
        }}
      >
        {/* Step 1: Basic Config */}
        <StepsForm.StepForm name="step1" title="基本配置">
          <ProFormText
            name="name"
            label="规则名称"
            placeholder="例如：每小时采集传感器数据"
            rules={[{ required: true, message: '请输入规则名称' }]}
          />

          <ProFormSelect
            name="mode"
            label="采集模式"
            initialValue="WATCH"
            options={[
              { label: 'Watch 模式（实时监控）', value: 'WATCH' },
              { label: 'Scheduled 模式（定时任务）', value: 'SCHEDULED' },
            ]}
            rules={[{ required: true }]}
            fieldProps={{
              onChange: (v) => setMode(v as CollectionMode),
            }}
          />

          <ProFormSelect
            name="bucket_id"
            label="目标 Bucket"
            options={bucketOptions}
            rules={[{ required: true, message: '请选择目标 Bucket' }]}
            placeholder="选择上传目标 Bucket"
          />
        </StepsForm.StepForm>

        {/* Step 2: Source Path Config */}
        <StepsForm.StepForm name="step2" title="源路径配置">
          <ProFormText
            name="base_path"
            label="源目录路径"
            placeholder="/data/sensors"
            rules={[
              { required: true, message: '请输入源路径' },
              {
                validator: (_, value) => {
                  if (value && !value.startsWith('/') && !/^[A-Za-z]:[\\/]/.test(value)) {
                    return Promise.reject('路径必须是绝对路径')
                  }
                  return Promise.resolve()
                },
              },
            ]}
          />

          <ProFormText
            name="path_pattern"
            label="文件过滤模式"
            placeholder="*.csv"
            initialValue="*"
            rules={[{ required: true, message: '请输入文件过滤模式' }]}
            tooltip="支持 glob（*.csv、**/*.csv）和 trollsift 结构化模式（如 {device}/{date:yyyy/MM/dd}/{filename}）"
          />

          <ProFormSwitch
            name="recursive"
            label="递归监控子目录"
            initialValue={false}
          />

          <ProFormSelect
            name="append_mode"
            label="上传模式"
            initialValue="overwrite"
            options={[
              { label: 'overwrite（全量）', value: 'overwrite' },
              { label: 'tail（追加尾部）', value: 'tail' },
              { label: 'close_wait（写完后上传）', value: 'close_wait' },
            ]}
          />

          {mode === 'SCHEDULED' && (
            <>
              <ProFormText
                name="cron_expr"
                label="Cron 表达式"
                placeholder="0 * * * *"
                rules={[
                  { required: true, message: '请输入 Cron 表达式' },
                  {
                    validator: (_, value) => {
                      if (value && value.trim().split(/\s+/).length !== 5) {
                        return Promise.reject('Cron 表达式需要 5 个字段')
                      }
                      return Promise.resolve()
                    },
                  },
                ]}
                fieldProps={{
                  onChange: (e) => setCronExpr(e.target.value),
                }}
              />
              {cronExpr && (
                <Alert
                  type="info"
                  message={describeCron(cronExpr)}
                  style={{ marginBottom: 16 }}
                  showIcon
                />
              )}

              <ProFormSwitch
                name="run_once_on_start"
                label="启动时立即执行一次"
                initialValue={false}
              />
            </>
          )}
        </StepsForm.StepForm>

        {/* Step 3: Upload Path Template */}
        <StepsForm.StepForm name="step3" title="上传路径配置">
          <Card size="small" title="可用模板变量" style={{ marginBottom: 16 }}>
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
              {Object.entries(PATH_TEMPLATE_VARIABLES).map(([key, desc]) => (
                <div key={key}>
                  <Text code>{key}</Text>
                  <Text type="secondary" style={{ marginLeft: 4, fontSize: 12 }}>
                    {desc}
                  </Text>
                </div>
              ))}
            </div>
          </Card>

          <ProFormText
            name="dest_path_template"
            label="上传路径模板"
            initialValue="/{year}/{month}/{agent_name}/{filename}"
            rules={[
              { required: true, message: '请输入路径模板' },
              {
                validator: (_, value) => {
                  const err = validatePathTemplate(value)
                  return err ? Promise.reject(err) : Promise.resolve()
                },
              },
            ]}
            fieldProps={{
              onChange: (e) => handlePathTemplateChange(e.target.value),
            }}
          />

          {pathError && (
            <Alert type="error" message={pathError} showIcon style={{ marginBottom: 12 }} />
          )}

          <Card size="small" style={{ backgroundColor: '#f5f5f5' }}>
            <Text type="secondary">路径预览：</Text>
            <br />
            <Text code style={{ fontSize: 13 }}>
              {renderPathPreview(pathTemplate)}
            </Text>
          </Card>
        </StepsForm.StepForm>
      </StepsForm>
    </div>
  )
}

export default AgentRuleFormPage
