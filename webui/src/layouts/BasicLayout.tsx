import { Link, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { ProLayout, DefaultFooter } from '@ant-design/pro-components'
import {
  DashboardOutlined,
  RobotOutlined,
  FileOutlined,
  TagsOutlined,
  InboxOutlined,
  BellOutlined,
  FileTextOutlined,
  SettingOutlined,
  LogoutOutlined,
} from '@ant-design/icons'
import { Dropdown, Avatar, theme } from 'antd'
import type { MenuDataItem } from '@ant-design/pro-components'
import useAuthStore from '../store/auth'

/** Navigation menu items for the sidebar. */
const menuData: MenuDataItem[] = [
  {
    path: '/dashboard',
    name: '仪表盘',
    icon: <DashboardOutlined />,
  },
  {
    path: '/agents',
    name: '采集器',
    icon: <RobotOutlined />,
    children: [
      { path: '/agents', name: '全部采集器' },
      { path: '/agents/pending', name: '待审批' },
    ],
  },
  {
    path: '/files',
    name: '文件',
    icon: <FileOutlined />,
  },
  {
    path: '/file-types',
    name: '文件类型',
    icon: <TagsOutlined />,
  },
  {
    path: '/buckets',
    name: 'Bucket',
    icon: <InboxOutlined />,
  },
  {
    path: '/events',
    name: '事件规则',
    icon: <BellOutlined />,
  },
  {
    path: '/logs',
    name: '日志',
    icon: <FileTextOutlined />,
  },
  {
    path: '/settings',
    name: '设置',
    icon: <SettingOutlined />,
    children: [
      { path: '/settings/users', name: '用户管理' },
      { path: '/settings/tag-keys', name: '标签词表' },
      { path: '/settings/pending-tags', name: '待确认取值' },
      { path: '/settings/profile', name: '个人信息' },
    ],
  },
]

/**
 * BasicLayout — ProLayout-based main application shell with sidebar navigation.
 * All authenticated pages are rendered inside the `<Outlet />`.
 */
function BasicLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const { user, logout } = useAuthStore()
  const { token } = theme.useToken()

  const handleLogout = () => {
    logout()
    navigate('/login')
  }

  const avatarDropdownItems = [
    {
      key: 'profile',
      label: <Link to="/settings/profile">个人信息</Link>,
    },
    {
      key: 'logout',
      label: (
        <span onClick={handleLogout} style={{ cursor: 'pointer' }}>
          <LogoutOutlined style={{ marginRight: 8 }} />
          退出登录
        </span>
      ),
    },
  ]

  return (
    <ProLayout
      title="FileAgent"
      logo={null}
      location={{ pathname: location.pathname }}
      menuDataRender={() => menuData}
      menuItemRender={(item, dom) => (
        <Link to={item.path ?? '/'}>{dom}</Link>
      )}
      // Fixed 208px sidebar, not collapsible (§1.4 / §3).
      siderWidth={208}
      collapsed={false}
      collapsedButtonRender={false}
      avatarProps={{
        src: undefined,
        title: (
          <Dropdown menu={{ items: avatarDropdownItems }} placement="bottomRight">
            <span style={{ cursor: 'pointer' }}>
              <Avatar size="small" style={{ marginRight: 8, backgroundColor: token.colorPrimary }}>
                {user?.username?.[0]?.toUpperCase() ?? 'U'}
              </Avatar>
              {user?.username}
            </span>
          </Dropdown>
        ),
      }}
      footerRender={() => (
        <DefaultFooter copyright="FileAgent" links={[]} />
      )}
      style={{ minHeight: '100vh' }}
    >
      <Outlet />
    </ProLayout>
  )
}

export default BasicLayout
