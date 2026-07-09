import type { ThemeConfig } from 'antd'

/**
 * Design tokens for the FileAgent Web UI redesign (Half A, WR-1).
 *
 * Single source of truth for color / typography / sizing. Mirrors
 * `docs/design/webui-redesign.md` §1.1 (色彩), §1.3 (字号阶梯) and §1.4 (尺寸与间距).
 * Injected once via `ConfigProvider theme={themeConfig}` in `main.tsx`.
 */

/** AntD semantic feedback colors (§1.1) — for buttons / alerts via token. */
export const semanticColors = {
  success: '#1B8A5A',
  warning: '#B27409',
  error: '#D64545',
} as const

/**
 * Status badge tones (§1.2) — the light-pill look: `{ bg, text(deep), dot(bright) }`.
 * Reproduces the mockup exactly (dot color ≠ text color: text is the deep shade,
 * dot the brighter one). Used by `StatusBadge`.
 */
export const statusBadgeTones = {
  success: { bg: '#E8F6EF', text: '#1B8A5A', dot: '#22A06B' },
  neutral: { bg: '#F2F3F5', text: '#646A73', dot: '#8F959E' },
  warning: { bg: '#FCF3E3', text: '#B27409', dot: '#D98D0B' },
  error: { bg: '#FBEDED', text: '#C03D3D', dot: '#D64545' },
  /** In-progress / informational (uploading / pending-delivery / approved) — no mockup ref, derived. */
  processing: { bg: '#EBF1FD', text: '#2F6BE0', dot: '#3D7BE8' },
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
    colorSuccess: semanticColors.success,
    colorWarning: semanticColors.warning,
    colorError: semanticColors.error,
    colorSplit: '#EEF0F3', // 分隔线：比 border 更浅（§1.1 divider）

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
      itemColor: '#4E5561', // 导航默认文字（§1.1 nav-text）
      itemSelectedColor: '#2F6BE0',
      itemSelectedBg: '#EBF1FD',
    },
    Table: {
      headerBg: '#FAFBFC', // 表头淡底（§1.1 subtle-bg）
      headerColor: '#646A73',
      headerSplitColor: 'transparent',
      borderColor: '#EEF0F3', // 行分隔线更浅（§1.1 divider）
      rowHoverBg: '#F5F6F8',
      cellPaddingBlock: 12, // ≈ 44px 行高 / 40px 表头
    },
    Card: {
      paddingLG: 20,
    },
  },
}

export default themeConfig
