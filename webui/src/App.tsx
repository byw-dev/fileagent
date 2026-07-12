import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { Suspense, lazy } from 'react'
import { Spin } from 'antd'
import BasicLayout from './layouts/BasicLayout'
import AuthLayout from './layouts/AuthLayout'
import AuthGuard from './components/AuthGuard'

// Lazy-load pages to enable code splitting
const LoginPage = lazy(() => import('./pages/Login'))
const DashboardPage = lazy(() => import('./pages/Dashboard'))
const AgentsPage = lazy(() => import('./pages/Agents'))
const AgentsPendingPage = lazy(() => import('./pages/Agents/Pending'))
const AgentDetailPage = lazy(() => import('./pages/Agents/Detail'))
const AgentRulesPage = lazy(() => import('./pages/Agents/Rules'))
const AgentRuleFormPage = lazy(() => import('./pages/Agents/RuleForm'))
const AgentLogsPage = lazy(() => import('./pages/Agents/Logs'))
const FilesPage = lazy(() => import('./pages/Files'))
const FileDetailPage = lazy(() => import('./pages/Files/Detail'))
const FileTypesPage = lazy(() => import('./pages/FileTypes'))
const FileTypeCreatePage = lazy(() => import('./pages/FileTypes/Create'))
const FileTypeDetailPage = lazy(() => import('./pages/FileTypes/Detail'))
const BucketsPage = lazy(() => import('./pages/Buckets'))
const EventsPage = lazy(() => import('./pages/Events'))
const EventCreatePage = lazy(() => import('./pages/Events/Create'))
const EventDeliveriesPage = lazy(() => import('./pages/Events/Deliveries'))
const LogsPage = lazy(() => import('./pages/Logs'))
const SettingsPage = lazy(() => import('./pages/Settings'))
const SettingsUsersPage = lazy(() => import('./pages/Settings/Users'))
const SettingsTagKeysPage = lazy(() => import('./pages/Settings/TagKeys'))
const SettingsPendingTagsPage = lazy(() => import('./pages/Settings/PendingTags'))
const ProfilePage = lazy(() => import('./pages/Settings/Profile'))

/** Full-screen centered loading spinner shown during lazy page loads.
 * Opaque bg-page background so the lazy fallback never exposes the UA canvas. */
function PageLoader() {
  return (
    <div
      style={{
        display: 'flex',
        justifyContent: 'center',
        alignItems: 'center',
        minHeight: '100vh',
        background: '#F5F6F8',
      }}
    >
      <Spin size="large" />
    </div>
  )
}

/**
 * App — root component that defines the React Router v6 route tree.
 *
 * Route structure:
 * - /login           — public, AuthLayout
 * - /*               — protected (AuthGuard), BasicLayout (ProLayout sidebar)
 */
function App() {
  return (
    <BrowserRouter>
      <Suspense fallback={<PageLoader />}>
        <Routes>
          {/* Public routes */}
          <Route element={<AuthLayout />}>
            <Route path="/login" element={<LoginPage />} />
          </Route>

          {/* Protected routes */}
          <Route element={<AuthGuard />}>
            <Route element={<BasicLayout />}>
              <Route index element={<Navigate to="/dashboard" replace />} />
              <Route path="/dashboard" element={<DashboardPage />} />

              {/* Agents */}
              <Route path="/agents" element={<AgentsPage />} />
              <Route path="/agents/pending" element={<AgentsPendingPage />} />
              <Route path="/agents/:id" element={<AgentDetailPage />} />
              <Route path="/agents/:id/rules" element={<AgentRulesPage />} />
              <Route path="/agents/:id/rules/create" element={<AgentRuleFormPage />} />
              <Route path="/agents/:id/rules/:rid/edit" element={<AgentRuleFormPage />} />
              <Route path="/agents/:id/logs" element={<AgentLogsPage />} />

              {/* Files */}
              <Route path="/files" element={<FilesPage />} />
              <Route path="/files/:id" element={<FileDetailPage />} />

              {/* File types */}
              <Route path="/file-types" element={<FileTypesPage />} />
              <Route path="/file-types/create" element={<FileTypeCreatePage />} />
              <Route path="/file-types/:id" element={<FileTypeDetailPage />} />

              {/* Buckets */}
              <Route path="/buckets" element={<BucketsPage />} />

              {/* Events */}
              <Route path="/events" element={<EventsPage />} />
              <Route path="/events/create" element={<EventCreatePage />} />
              <Route path="/events/:id/deliveries" element={<EventDeliveriesPage />} />

              {/* Logs */}
              <Route path="/logs" element={<LogsPage />} />

              {/* Settings */}
              <Route path="/settings" element={<SettingsPage />} />
              <Route path="/settings/users" element={<SettingsUsersPage />} />
              <Route path="/settings/tag-keys" element={<SettingsTagKeysPage />} />
              <Route path="/settings/pending-tags" element={<SettingsPendingTagsPage />} />
              <Route path="/settings/profile" element={<ProfilePage />} />
            </Route>
          </Route>

          {/* Catch-all */}
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </Suspense>
    </BrowserRouter>
  )
}

export default App
