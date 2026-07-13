import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import {
  App,
  Typography,
  Button,
  Space,
  Alert,
  Card,
  Collapse,
  Table,
  Spin,
  Empty,
} from 'antd'
import {
  StepsForm,
  ProFormText,
  ProFormSelect,
  ProFormSwitch,
  ProFormList,
} from '@ant-design/pro-components'
import { useParams, useNavigate } from 'react-router-dom'
import { createRule, updateRule, listRules, testRule } from '../../services/agents'
import type { CollectionMode, TestRuleFileResult } from '../../services/agents'
import { listTagKeys } from '../../services/tags'
import { toRuleMetadata, metadataToFormFields, templateVarName, pathTemplateVars } from './ruleMetadata'
import type { StaticTagRow, PathTagRow } from './ruleMetadata'
import apiClient from '../../services/api'
import {
  renderPathPreview,
  validatePathTemplate,
  extractDynamicFields,
  SYSTEM_TEMPLATE_VARIABLES,
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

/** Flat form values merged by StepsForm.onFinish across all 4 steps. */
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
  // Metadata step (6c). Maps are edited as arrays of rows, converted on submit.
  file_type?: string
  static_tags?: StaticTagRow[]
  path_tag_map?: PathTagRow[]
}

/**
 * Agent rule creation form — 4-step ProForm.
 * Step 1: Basic config (name, mode, target bucket).
 * Step 2: Source path config (Watch vs Scheduled fields differ).
 * Step 3: Upload path template with live preview.
 * Step 4: Metadata (6c) — declared file type + static/path-derived tags.
 */
/** Default form values used for creation and as the base for editing. */
const DEFAULT_VALUES: RuleFormValues = {
  name: '',
  mode: 'WATCH',
  bucket_id: '',
  base_path: '',
  path_pattern: '*',
  cron_expr: '',
  run_once_on_start: false,
  recursive: false,
  append_mode: 'overwrite',
  enabled: true,
  dest_path_template: '/{agent_name}/{time:yyyy/MM/dd}/{filename}',
  file_type: '',
  static_tags: [],
  path_tag_map: [],
}

function AgentRuleFormPage() {
  const { id: agentId, rid } = useParams<{ id: string; rid?: string }>()
  const isEdit = Boolean(rid)
  const navigate = useNavigate()
  const { message } = App.useApp()

  // Load the tag-key vocabulary once (SWR-cached) rather than per-select. Static
  // tags may use any key; path-tag-map only keys that allow path-variable mapping
  // (the indexer skips allow_path_var=false keys, so offering them would create a
  // rule that silently does nothing).
  const { data: tagKeys = [] } = useSWR('tag-keys', listTagKeys)
  const staticKeyOptions = useMemo(
    () => tagKeys.map((k) => ({ label: `${k.key}（${k.label}）`, value: k.key })),
    [tagKeys],
  )
  const pathKeyOptions = useMemo(
    () =>
      tagKeys
        .filter((k) => k.allow_path_var)
        .map((k) => ({ label: `${k.key}（${k.label}）`, value: k.key })),
    [tagKeys],
  )

  // In edit mode the existing rule is fetched before rendering the form so the
  // steps can be prefilled; creation starts from DEFAULT_VALUES immediately.
  const [loading, setLoading] = useState(isEdit)
  const [initial, setInitial] = useState<RuleFormValues>(DEFAULT_VALUES)

  const [mode, setMode] = useState<CollectionMode>('WATCH')
  const [cronExpr, setCronExpr] = useState('')
  const [pathTemplate, setPathTemplate] = useState(DEFAULT_VALUES.dest_path_template)
  const [pathError, setPathError] = useState<string | null>(null)
  const [pathPattern, setPathPattern] = useState('*')
  const [basePath, setBasePath] = useState('')
  const [recursive, setRecursive] = useState(false)
  const [buckets, setBuckets] = useState<Bucket[]>([])
  const [submitting, setSubmitting] = useState(false)

  // Edit mode: load the rule and prefill both the form initial values and the
  // local state the previews/conditionals depend on.
  useEffect(() => {
    if (!isEdit || !agentId || !rid) return
    let cancelled = false
    ;(async () => {
      try {
        const res = await listRules(agentId)
        const rule = res.items.find((r) => r.id === rid)
        if (cancelled) return
        if (!rule) {
          message.error('规则不存在')
          navigate(`/agents/${agentId}`, { state: { tab: 'rules' } })
          return
        }
        // The API returns mode lowercase; the select expects WATCH/SCHEDULED.
        const ruleMode = (rule.mode ?? 'WATCH').toUpperCase() as CollectionMode
        setInitial({
          name: rule.name,
          mode: ruleMode,
          bucket_id: rule.bucket_id,
          base_path: rule.base_path,
          path_pattern: rule.path_pattern,
          cron_expr: rule.cron_expr ?? '',
          run_once_on_start: rule.run_once_on_start,
          recursive: rule.recursive,
          append_mode: rule.append_mode || 'overwrite',
          enabled: rule.enabled,
          dest_path_template: rule.dest_path_template,
          ...metadataToFormFields(rule.metadata),
        })
        setMode(ruleMode)
        setBasePath(rule.base_path)
        setPathPattern(rule.path_pattern)
        setPathTemplate(rule.dest_path_template)
        setRecursive(rule.recursive)
        setCronExpr(rule.cron_expr ?? '')
      } catch {
        if (!cancelled) {
          message.error('加载规则失败')
          navigate(`/agents/${agentId}`, { state: { tab: 'rules' } })
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [isEdit, agentId, rid, navigate, message])

  const [testLoading, setTestLoading] = useState(false)
  const [testFiles, setTestFiles] = useState<TestRuleFileResult[] | null>(null)
  const [testError, setTestError] = useState<{ type: 'warning' | 'error'; message: string } | null>(null)

  const handleTestRule = async () => {
    if (!agentId) return
    setTestLoading(true)
    setTestFiles(null)
    setTestError(null)
    try {
      const result = await testRule(agentId, {
        base_path: basePath,
        path_pattern: pathPattern,
        dest_path_template: pathTemplate,
        recursive,
        dry_run_limit: 10,
      })
      setTestFiles(result.files)
    } catch (err: unknown) {
      const status = (err as { response?: { status?: number; data?: { error?: { message?: string } } } })?.response?.status
      const errMsg = (err as { response?: { data?: { error?: { message?: string } } } })?.response?.data?.error?.message
      if (status === 409) {
        setTestError({ type: 'warning', message: '采集器当前离线，无法预览' })
      } else if (status === 504) {
        setTestError({ type: 'error', message: '采集器响应超时（30s）' })
      } else if (status === 422) {
        setTestError({ type: 'error', message: errMsg ?? '采集模式格式错误' })
      } else {
        setTestError({ type: 'error', message: '测试失败，请重试' })
      }
    } finally {
      setTestLoading(false)
    }
  }

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
      const payload = {
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
        // Editing preserves the rule's enabled state (managed via the list
        // toggle); creation defaults to enabled.
        enabled: isEdit ? (initial.enabled ?? true) : true,
        // Declared metadata (6c). Always sent so an edit round-trips it rather
        // than the backend defaulting a missing value to {}.
        metadata: toRuleMetadata(values),
      }
      if (isEdit && rid) {
        await updateRule(agentId, rid, payload)
        message.success('规则更新成功')
      } else {
        await createRule(agentId, payload)
        message.success('规则创建成功')
      }
      navigate(`/agents/${agentId}`, { state: { tab: 'rules' } })
      return true
    } catch {
      message.error(isEdit ? '规则更新失败，请稍后重试' : '规则创建失败，请稍后重试')
      return false
    } finally {
      setSubmitting(false)
    }
  }

  const bucketOptions = buckets.map((b) => ({ label: b.name, value: b.id }))

  if (isEdit && loading) {
    return (
      <div style={{ maxWidth: 760, margin: '0 auto', textAlign: 'center', paddingTop: 80 }}>
        <Spin />
      </div>
    )
  }

  return (
    <div style={{ maxWidth: 760, margin: '0 auto' }}>
      <Space style={{ marginBottom: 24 }}>
        <Button onClick={() => navigate(`/agents/${agentId}`)}>← 返回</Button>
        <Title level={4} style={{ margin: 0 }}>{isEdit ? '编辑采集规则' : '新建采集规则'}</Title>
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
            // Middle steps (source path, upload path): back + next.
            if (props.step === 1 || props.step === 2) {
              return (
                <Space>
                  <Button onClick={() => props.onPre?.()}>上一步</Button>
                  <Button type="primary" onClick={() => props.onSubmit?.()}>
                    下一步
                  </Button>
                </Space>
              )
            }
            // Last step (step 3: metadata): back + submit.
            return (
              <Space>
                <Button onClick={() => props.onPre?.()}>上一步</Button>
                <Button type="primary" loading={submitting} onClick={() => props.onSubmit?.()}>
                  {isEdit ? '保存修改' : '创建规则'}
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
            initialValue={initial.name}
            rules={[{ required: true, message: '请输入规则名称' }]}
          />

          <ProFormSelect
            name="mode"
            label="采集模式"
            initialValue={initial.mode}
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
            initialValue={initial.bucket_id || undefined}
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
            initialValue={initial.base_path}
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
            fieldProps={{
              onChange: (e) => setBasePath(e.target.value),
            }}
          />

          <ProFormText
            name="path_pattern"
            label="文件过滤模式"
            placeholder="*.csv"
            initialValue={initial.path_pattern}
            rules={[{ required: true, message: '请输入文件过滤模式' }]}
            tooltip="支持 glob（*.csv、**/*.csv）和 trollsift 结构化模式（如 {device}/{date:yyyy/MM/dd}/{filename}）"
            fieldProps={{
              onChange: (e) => setPathPattern(e.target.value),
            }}
          />

          <ProFormSwitch
            name="recursive"
            label="递归监控子目录"
            initialValue={initial.recursive}
            fieldProps={{
              onChange: (checked) => setRecursive(checked),
            }}
          />

          <ProFormSelect
            name="append_mode"
            label="上传模式"
            initialValue={initial.append_mode}
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
                initialValue={initial.cron_expr}
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
                initialValue={initial.run_once_on_start}
              />
            </>
          )}
        </StepsForm.StepForm>

        {/* Step 3: Upload Path Template */}
        <StepsForm.StepForm name="step3" title="上传路径配置">
          <Card size="small" title="可用模板变量" style={{ marginBottom: 16 }}>
            <div>
              <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>
                系统变量（始终可用）：
              </Text>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 8 }}>
                {SYSTEM_TEMPLATE_VARIABLES.map(({ key, desc }) => (
                  <div key={key}>
                    <Text code>{key}</Text>
                    <Text type="secondary" style={{ marginLeft: 4, fontSize: 12 }}>{desc}</Text>
                  </div>
                ))}
                <div>
                  <Text code>{'{字段名:LDML格式}'}</Text>
                  <Text type="secondary" style={{ marginLeft: 4, fontSize: 12 }}>
                    时间字段，例如 {'{'}{`time:yyyy/MM/dd`}{'}'} 、{'{'}{`ts:HH:mm:ss|tz=Asia/Shanghai`}{'}'}
                  </Text>
                </div>
              </div>
              {(() => {
                const dynamicFields = extractDynamicFields(pathPattern)
                return dynamicFields.length > 0 ? (
                  <div>
                    <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>
                      来自文件过滤模式（path_pattern）的字段：
                    </Text>
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
                      {dynamicFields.map((f) => (
                        <Text code key={f}>{`{${f}}`}</Text>
                      ))}
                    </div>
                  </div>
                ) : null
              })()}
            </div>
          </Card>

          <ProFormText
            name="dest_path_template"
            label="上传路径模板"
            initialValue={initial.dest_path_template}
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
              {renderPathPreview(pathTemplate, extractDynamicFields(pathPattern))}
            </Text>
          </Card>

          <Collapse
            style={{ marginTop: 16 }}
            items={[{
              key: 'test',
              label: '规则测试（在线预览）',
              children: (
                <Space direction="vertical" style={{ width: '100%' }}>
                  <Button
                    type="primary"
                    loading={testLoading}
                    onClick={handleTestRule}
                    disabled={!basePath || !pathPattern || !pathTemplate}
                  >
                    立即测试
                  </Button>
                  {testLoading && <Spin />}
                  {testError && (
                    <Alert type={testError.type} message={testError.message} showIcon />
                  )}
                  {!testLoading && !testError && testFiles !== null && (
                    testFiles.length === 0
                      ? <Empty description="未找到匹配文件" />
                      : (
                        <Table<TestRuleFileResult>
                          size="small"
                          dataSource={testFiles}
                          rowKey="local_path"
                          pagination={false}
                          columns={[
                            { title: '本地路径', dataIndex: 'local_path', ellipsis: true },
                            {
                              title: '解析字段',
                              dataIndex: 'parsed_fields',
                              render: (fields: Record<string, string>) =>
                                Object.entries(fields).map(([k, v]) => `${k}=${v}`).join(', ') || '—',
                              ellipsis: true,
                            },
                            { title: '上传路径', dataIndex: 'upload_path', ellipsis: true },
                            {
                              title: '状态',
                              dataIndex: 'compose_error',
                              render: (e: string) => e
                                ? <Alert type="error" message={e} banner />
                                : <Text type="success">✓</Text>,
                            },
                          ]}
                        />
                      )
                  )}
                </Space>
              ),
            }]}
          />
        </StepsForm.StepForm>

        {/* Step 4: Metadata (6c) — declared file type + static/path-derived tags.
            Uses per-field initialValue like the earlier steps; a StepForm-level
            initialValues with the full object would let this step contribute (and
            overwrite) earlier steps' fields when StepsForm merges values. */}
        <StepsForm.StepForm<RuleFormValues> name="step4" title="元数据">
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 16 }}
            message="声明该规则采集文件的元数据"
            description="声明类型优先于按扩展名的兜底分类；静态标签打在每个文件上；路径标签从上传路径模板变量提取取值。"
          />
          <ProFormText
            name="file_type"
            label="声明类型"
            placeholder="例如 pressure / vibration（留空则按扩展名兜底）"
            tooltip="规则声明的粗分类，优先于 glob 兜底"
            initialValue={initial.file_type}
          />
          <ProFormList
            name="static_tags"
            label="静态标签"
            initialValue={initial.static_tags}
            creatorButtonProps={{ creatorButtonText: '添加静态标签' }}
            copyIconProps={false}
          >
            <Space align="baseline">
              <ProFormSelect
                name="key"
                placeholder="标签键"
                width="sm"
                showSearch
                options={staticKeyOptions}
                rules={[{ required: true, message: '请选择标签键' }]}
              />
              <ProFormText
                name="value"
                placeholder="取值（未登记的受控取值将入待确认队列）"
                width="md"
                rules={[{ required: true, whitespace: true, message: '请输入取值' }]}
              />
            </Space>
          </ProFormList>
          <ProFormList
            name="path_tag_map"
            label="路径标签映射"
            initialValue={initial.path_tag_map}
            tooltip="把上传路径模板中的变量映射到标签键，例如变量 {site} → 标签键 site（仅列出允许路径变量的键）"
            creatorButtonProps={{ creatorButtonText: '添加路径标签' }}
            copyIconProps={false}
          >
            <Space align="baseline">
              <ProFormSelect
                name="key"
                placeholder="标签键"
                width="sm"
                showSearch
                options={pathKeyOptions}
                rules={[{ required: true, message: '请选择标签键' }]}
              />
              <ProFormText
                name="template"
                placeholder="路径变量，例如 {site}"
                width="md"
                rules={[
                  { required: true, whitespace: true, message: '请输入路径变量' },
                  {
                    // Validate the trimmed value (it is stored trimmed) as a single
                    // {var}/{var:fmt}, and require that var to appear in the upload
                    // path template — the indexer skips path vars not present there,
                    // so the mapping would silently do nothing.
                    validator: (_, value?: string) => {
                      const name = templateVarName(value ?? '')
                      if (!value?.trim()) return Promise.resolve()
                      if (!name) {
                        return Promise.reject(new Error('需为单个模板变量，例如 {site} 或 {site:fmt}'))
                      }
                      if (!pathTemplateVars(pathTemplate).has(name)) {
                        return Promise.reject(new Error(`变量 {${name}} 未出现在上传路径模板中，将不会生效`))
                      }
                      return Promise.resolve()
                    },
                  },
                ]}
              />
            </Space>
          </ProFormList>
        </StepsForm.StepForm>
      </StepsForm>
    </div>
  )
}

export default AgentRuleFormPage
