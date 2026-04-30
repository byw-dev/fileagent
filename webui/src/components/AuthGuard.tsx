import { Navigate, Outlet } from 'react-router-dom'
import useAuthStore from '../store/auth'

/**
 * AuthGuard — route wrapper that redirects unauthenticated users to /login.
 * Wraps all protected routes that require a valid access token.
 */
function AuthGuard() {
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated)

  if (!isAuthenticated) {
    return <Navigate to="/login" replace />
  }

  return <Outlet />
}

export default AuthGuard
