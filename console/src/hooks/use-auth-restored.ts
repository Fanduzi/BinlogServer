// Run a callback when a credential is saved after a 401 (see AUTH_RESTORED_EVENT).
import { useEffect, useRef } from 'react'
import { AUTH_RESTORED_EVENT } from '@/lib/api'

export function useOnAuthRestored(cb: () => void) {
  const ref = useRef(cb)
  ref.current = cb
  useEffect(() => {
    const on = () => ref.current()
    window.addEventListener(AUTH_RESTORED_EVENT, on)
    return () => window.removeEventListener(AUTH_RESTORED_EVENT, on)
  }, [])
}
