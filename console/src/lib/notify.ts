// Toasts for failed requests. A 401 is not toasted: the API layer already
// opens Settings with the re-auth banner, and a second message would only repeat it.
import { toast } from 'sonner'
import { errorText, errorTextAsync, isUnauthorized } from './errors'

export function notifyError(err: unknown, fallback?: string) {
  if (isUnauthorized(err)) return
  toast.error(errorText(err, fallback))
}

export async function notifyErrorAsync(err: unknown, fallback?: string) {
  if (isUnauthorized(err)) return
  toast.error(await errorTextAsync(err, fallback))
}
