import type { ThemeConfig } from 'antd'

/**
 * Design tokens for the FileAgent Web UI redesign (Half A, WR-1).
 *
 * Single source of truth for color / typography / sizing. Mirrors
 * `docs/design/webui-redesign.md` §1.1 (色彩), §1.3 (字号阶梯) and §1.4 (尺寸与间距).
 * Injected once via `ConfigProvider theme={themeConfig}` in `main.tsx`.
 */

/** Semantic status palette (§1.1 状态语义色) — strong color + light background pairs. */
export const statusColors = {
  success: { color: '#1B8A5A', bg: '#E8F6EF' },
  warning: { color: '#B27409', bg: '#FCF3E3' },
  error: { color: '#D64545', bg: '#FBEDED' },
  /** Neutral/muted (offline, inactive, deleted). */
  neutral: { color: '#8F959E', bg: '#F2F3F5' },
  /** In-progress / informational — reuses the brand primary. */
  processing: { color: '#2F6BE0', bg: '#EBF1FD' },
} as const

/** Font stack shared by CSS `:root` and the AntD token. */
export const fontFamily =
  '-apple-system, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif'

/** AntD 5 theme configuration — the site-wide single source of truth. */
export const themeConfig: ThemeConfig = {
  token: {
    // §1.1 色彩
    colorPrimary: '#2F6BE0',
    colorPrimaryBg: '#EBF1FD',
    colorText: '#1F2329',
    colorTextSecondary: '#646A73',
    colorTextTertiary: '#8F959E',
    colorTextPlaceholder: '#8F959E',
    colorBorder: '#E3E6EB',
    colorBorderSecondary: '#E3E6EB',
    colorBgLayout: '#F5F6F8',
    colorSuccess: statusColors.success.color,
    colorWarning: statusColors.warning.color,
    colorError: statusColors.error.color,

    // §1.3 字号阶梯（root = 16px；token 用 px 数值）
    fontFamily,
    fontSize: 13, // 正文 / 表格 / 控件 (0.8125rem)
    fontSizeLG: 15, // 卡片标题 (0.9375rem)
    fontSizeSM: 12, // 辅助 / 表头 / 徽标 (0.75rem)

    // §1.4 尺寸与间距
    controlHeight: 32,
    borderRadius: 6,
    borderRadiusLG: 8,
  },
  components: {
    Layout: {
      bodyBg: '#F5F6F8',
      siderBg: '#FFFFFF',
    },
    Menu: {
      itemSelectedColor: '#2F6BE0',
      itemSelectedBg: '#EBF1FD',
    },
    Table: {
      headerBg: '#F5F6F8',
      headerColor: '#646A73',
      headerSplitColor: 'transparent',
      borderColor: '#E3E6EB',
      rowHoverBg: '#F5F6F8',
      cellPaddingBlock: 12, // ≈ 44px 行高 / 40px 表头
    },
    Card: {
      paddingLG: 20,
    },
  },
}

export default themeConfig
