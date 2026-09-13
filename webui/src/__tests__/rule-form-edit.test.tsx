/**
 * Tests for AgentRuleFormPage edit mode (CC-9 part 2).
 * - Edit mode fetches the existing rule and prefills the wizard.
 * - Create mode does not fetch and shows the create title.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { App } from 'antd'

const mockListRules = vi.fn()
const mockCreateRule = vi.fn()
const mockUpdateRule = vi.fn()

vi.mock('../services/agents', () => ({
  listRules: (...a: unknown[]) => mockListRules(...a),
  createRule: (...a: unknown[]) => mockCreateRule(...a),
  updateRule: (...a: unknown[]) => mockUpdateRule(...a),
  testRule: vi.fn(),
}))

// RuleForm loads buckets via apiClient directly.
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
  mode: 'scheduled', // API returns lowercase
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

describe('AgentRuleFormPage edit mode', () => {
  beforeEach(() => vi.clearAllMocks())

  it('fetches the rule and prefills the form in edit mode', async () => {
    mockListRules.mockResolvedValue({ items: [existingRule] })
    renderAt(`/agents/${AGENT_ID}/rules/${RULE_ID}/edit`)

    // Title reflects edit mode and the rule name is prefilled (step 1 field).
    expect(await screen.findByText('编辑采集规则')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByDisplayValue('nightly-sync')).toBeInTheDocument())
    expect(mockListRules).toHaveBeenCalledWith(AGENT_ID)
  })

  it('does not fetch and shows the create title in create mode', async () => {
    renderAt(`/agents/${AGENT_ID}/rules/create`)
    expect(await screen.findByText('新建采集规则')).toBeInTheDocument()
    expect(mockListRules).not.toHaveBeenCalled()
  })

  // IC-BUG-46 fail-closed: the tail option must be selectable-by-sight only —
  // selecting it would always end in a CP 422, so the UI disables it and
  // states why instead of silently removing it.
  it('disables the tail append-mode option and states the reason', async () => {
    mockListRules.mockResolvedValue({ items: [existingRule] })
    renderAt(`/agents/${AGENT_ID}/rules/${RULE_ID}/edit`)

    expect(await screen.findByText('编辑采集规则')).toBeInTheDocument()

    // Append mode lives in step 2; advance past step 1.
    fireEvent.click(screen.getByRole('button', { name: '下一步' }))
    await waitFor(() => expect(screen.getByLabelText('上传模式')).toBeInTheDocument())

    // Open the append-mode dropdown (antd opens on mousedown on the selector).
    const select = document.querySelector('#step2_append_mode')?.closest('.ant-select')
    expect(select).not.toBeNull()
    fireEvent.mouseDown(select!.querySelector('.ant-select-selector')!)

    // The tail option is rendered but disabled, with a readable reason.
    const tailOption = await screen.findByText(
      (content, el) =>
        el?.classList.contains('ant-select-item-option-content') === true &&
        content.startsWith('tail（追加尾部）'),
    )
    const optionItem = tailOption.closest('.ant-select-item-option')
    expect(optionItem).not.toBeNull()
    expect(optionItem?.className).toContain('ant-select-item-option-disabled')
    expect(optionItem?.getAttribute('aria-selected')).toBe('false')
    expect(tailOption.textContent).toContain('已停用')
  })
})
