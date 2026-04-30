import { describe, it, expect } from 'vitest'
import {
  renderPathPreview,
  validatePathTemplate,
  PATH_TEMPLATE_VARIABLES,
} from '../utils/pathTemplate'

describe('renderPathPreview', () => {
  it('should replace date variables with current date values', () => {
    const preview = renderPathPreview('/{year}/{month}/{day}')
    const now = new Date()
    expect(preview).toContain(String(now.getUTCFullYear()))
    expect(preview).toMatch(/\/\d{4}\/\d{2}\/\d{2}/)
  })

  it('should replace agent variables with example strings', () => {
    const preview = renderPathPreview('/{agent_name}/{file_type}/{filename}')
    expect(preview).toContain('my-agent')
    expect(preview).toContain('var_hourly')
    expect(preview).toContain('data_20250415.csv')
  })

  it('should return template unchanged when no variables are used', () => {
    const preview = renderPathPreview('/static/path/to/file')
    expect(preview).toBe('/static/path/to/file')
  })

  it('should handle empty string input', () => {
    const preview = renderPathPreview('')
    expect(preview).toBe('')
  })
})

describe('validatePathTemplate', () => {
  it('should return null for a valid template', () => {
    expect(validatePathTemplate('/{year}/{month}/{agent_name}/{filename}')).toBeNull()
  })

  it('should reject empty string', () => {
    expect(validatePathTemplate('')).toBeTruthy()
  })

  it('should reject templates not starting with /', () => {
    expect(validatePathTemplate('year/month')).toBeTruthy()
  })

  it('should reject templates with double slashes', () => {
    expect(validatePathTemplate('//year/month')).toBeTruthy()
  })

  it('should reject unknown template variables', () => {
    const error = validatePathTemplate('/{year}/{unknown_var}')
    expect(error).toContain('{unknown_var}')
  })

  it('should accept all known template variables', () => {
    const knownVars = Object.keys(PATH_TEMPLATE_VARIABLES)
    const template = '/' + knownVars.join('/')
    expect(validatePathTemplate(template)).toBeNull()
  })
})
