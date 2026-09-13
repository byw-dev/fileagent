/**
 * IC-BUG-50 / review E1: the CP returns contract warnings (deprecated {time}
 * reserved word, reserved-word misuse, invalid timezone) on rule create/update.
 * The webui must render them — before this, the service layer dropped the
 * field entirely and the admin saw "saved successfully" with no signal.
 */
import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { App } from 'antd'

const mockListRules = vi.fn()
const mockUpdateRule = vi.fn()
const mockCreateRule = vi.fn()

vi.mock('../services/agents', () => ({
  listRules: (...a: unknown[]) => mockListRules(...a),
  createRule: (...a: unknown[]) => mockCreateRule(...a),
  updateRule: (...a: unknown[]) => mockUpdateRule(...a),
  testRule: vi.fn(),
}))

vi.mock('../services/api', () => ({
  default: { get: vi.fn().mockResolvedValue({ data: { items: [{ id: 'b-1', name: 'bucket-one' }] } }) },
}))

import AgentRuleFormPage from '../pages/Agents/RuleForm'

const AGENT_ID = 'agent-1'
const RULE_ID = 'rule-1'

const existingRule = {
  id: RULE_ID,
  agent_id: AGENT_ID,
  name: 'nightly-sync',
  mode: 'scheduled',
  base_path: '/data/nightly',
  path_pattern: '*.csv',
  cron_expr: '0 2 * * *',
  run_once_on_start: true,
  bucket_id: 'b-1',
  dest_path_template: 'out/{filename}',
  recursive: true,
  append_mode: 'overwrite',
  enabled: true,
  created_at: '2026-05-01T00:00:00Z',
}

const DEPRECATED_HINT =
  'dest_path_template uses the deprecated reserved word {time}; it still renders (the file submit-for-upload instant, unless path_pattern parses a field with that name — parse results always win), but new rules should use {submit_time}'

function renderAt(path: string) {
  return render(
    <App>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/agents/:id/rules/create" element={<AgentRuleFormPage />} />
          <Route path="/agents/:id/rules/:rid/edit" element={<AgentRuleFormPage />} />
        </Routes>
      </MemoryRouter>
    </App>,
  )
}

function activeStep(): string | null {
  const el = document.querySelector('.ant-steps-item-active .ant-steps-item-title')
  return el ? el.textContent : null
}

// The wizard has 4 steps: 基本配置 → 源路径配置 → 上传路径配置 → 元数据.
// Non-current steps stay in the DOM (hidden), so label lookups cannot be used
// to detect step changes — track the Steps indicator instead. The last step's
// submitter button is 保存修改.
async function openEditAndSubmit() {
  mockListRules.mockResolvedValue({ items: [existingRule] })
  renderAt(`/agents/${AGENT_ID}/rules/${RULE_ID}/edit`)
  expect(await screen.findByText('编辑采集规则')).toBeInTheDocument()
  const titles = ['基本配置', '源路径配置', '上传路径配置']
  for (const t of titles) {
    await waitFor(() => expect(activeStep()).toBe(t))
    fireEvent.click(screen.getByRole('button', { name: '下一步' }))
  }
  await waitFor(() => expect(activeStep()).toBe('元数据'))
  fireEvent.click(await screen.findByRole('button', { name: '保存修改' }))
}

describe('RuleForm: contract warnings are rendered after a successful save (IC-BUG-50 / review E1)', () => {
  it('renders the CP warnings block and stays on the form when update returns warnings', async () => {
    mockUpdateRule.mockResolvedValue({ ...existingRule, warnings: [DEPRECATED_HINT] })
    openEditAndSubmit()

    // The wizard's async form validation chain can take longer than the
    // default 1s findByText timeout on a 4-step StepsForm.
    // The warning is visible on the form itself — the admin can read it in
    // full, dismiss it, fix the template and re-save.
    expect(
      await screen.findByText(/dest_path_template uses the deprecated reserved word/, undefined, { timeout: 3000 }),
    ).toBeInTheDocument()
    // Staying on the form: the edit title is still mounted (a navigate away
    // would unmount the form and lose the readable hint).
    expect(screen.getByText('编辑采集规则')).toBeInTheDocument()
    expect(mockUpdateRule).toHaveBeenCalledTimes(1)
  })

  // create and update share the same handleFinish rendering branch (the
  // resp.warnings check is one code path); the CP-side create response is
  // covered by TestAgentsHandler_CreateRule_DeprecatedTimeTemplate_Warns.
  it('keeps the old behavior: no warnings navigates back to the rules tab', async () => {
    mockUpdateRule.mockResolvedValue({ ...existingRule })
    await openEditAndSubmit()
    // No warnings → the form navigates away (unmounts), as before.
    await waitFor(() => expect(screen.queryByText('编辑采集规则')).toBeNull())
  })
})