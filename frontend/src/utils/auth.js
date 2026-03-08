// input: localStorage API for token persistence
// output: getAuthToken, setAuthToken, clearAuthToken, hasAuthToken, getAuthHeaders
// pos: authentication token management for API requests
// note: if this file changes, update this header and frontend/README.md

/**
 * Authentication utilities for Binlog Server frontend.
 * Manages API token storage and request authentication.
 */

const TOKEN_KEY = "binlog_server_token";

/**
 * Get the stored auth token.
 * @returns {string|null}
 */
export function getAuthToken() {
  return localStorage.getItem(TOKEN_KEY);
}

/**
 * Set the auth token.
 * @param {string} token
 */
export function setAuthToken(token) {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_KEY);
  }
}

/**
 * Clear the stored auth token.
 */
export function clearAuthToken() {
  localStorage.removeItem(TOKEN_KEY);
}

/**
 * Check if auth token is configured.
 * @returns {boolean}
 */
export function hasAuthToken() {
  const token = getAuthToken();
  return token !== null && token.trim() !== "";
}

/**
 * Get authorization headers for API requests.
 * @returns {Object}
 */
export function getAuthHeaders() {
  const token = getAuthToken();
  if (!token) {
    return {};
  }
  return {
    Authorization: `Bearer ${token}`,
  };
}
