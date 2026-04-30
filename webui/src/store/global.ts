import { create } from 'zustand'

/** Notification item */
export interface Notification {
  id: string
  type: 'success' | 'error' | 'warning' | 'info'
  message: string
  description?: string
}

/** Global store state and actions */
export interface GlobalStore {
  /** Global loading state */
  loading: boolean
  /** Pending notifications queue */
  notifications: Notification[]
  /** Set global loading state */
  setLoading: (loading: boolean) => void
  /** Add a notification */
  addNotification: (notification: Omit<Notification, 'id'>) => void
  /** Remove a notification by id */
  removeNotification: (id: string) => void
  /** Clear all notifications */
  clearNotifications: () => void
}

const useGlobalStore = create<GlobalStore>((set) => ({
  loading: false,
  notifications: [],

  setLoading: (loading: boolean) => set({ loading }),

  addNotification: (notification) =>
    set((state) => ({
      notifications: [
        ...state.notifications,
        { ...notification, id: crypto.randomUUID() },
      ],
    })),

  removeNotification: (id: string) =>
    set((state) => ({
      notifications: state.notifications.filter((n) => n.id !== id),
    })),

  clearNotifications: () => set({ notifications: [] }),
}))

export default useGlobalStore
