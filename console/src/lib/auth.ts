// Same localStorage key as the Vue console (frontend/src/utils/auth.js),
// so a token saved in either console works in both.
const TOKEN_KEY = 'binlog_server_token'

export function getAuthToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setAuthToken(token: string) {
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

export function clearAuthToken() {
  localStorage.removeItem(TOKEN_KEY)
}
