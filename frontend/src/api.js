// input: axios HTTP client, utils/auth.js token storage, shared frontend mock handler, backend 401 responses
// output: API request helpers plus auth-required event dispatch for real and mock-backed settings flows
// pos: frontend API layer with auth interceptors and opt-in dev mock dispatch for backend communication
// note: keep 401 handling aligned with in-app settings guidance; update frontend/README.md if responsibilities change

import axios from "axios";
import { getAuthToken, clearAuthToken } from "./utils/auth.js";
import { createMockSession } from "./mocks/mock-handler.js";

const http = axios.create({
  baseURL: "/",
  timeout: 10000,
});

const devEnv = resolveDevEnv();
const useMockAPI = devEnv.VITE_USE_MOCK === "true";
const mockScenario = devEnv.VITE_MOCK_SCENARIO || "healthy";
const mockSession = useMockAPI ? createMockSession({ scenario: mockScenario }) : null;

// Request interceptor: add auth header if token is configured.
http.interceptors.request.use(
  (config) => {
    const token = getAuthToken();
    if (token) {
      config.headers.Authorization = `Bearer ${token}`;
    }
    return config;
  },
  (error) => Promise.reject(error),
);

// Response interceptor: handle 401 errors.
http.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error.response?.status === 401) {
      handleUnauthorized();
    }
    return Promise.reject(error);
  },
);

function resolveDevEnv() {
  const viteEnv =
    typeof import.meta !== "undefined" && import.meta.env
      ? import.meta.env
      : {};
  const overrideEnv =
    typeof globalThis !== "undefined" && globalThis.__BINLOG_DEV_ENV__
      ? globalThis.__BINLOG_DEV_ENV__
      : {};
  return {
    ...viteEnv,
    ...overrideEnv,
  };
}

function handleUnauthorized() {
  clearAuthToken();
  if (!window.__authDialogShown) {
    window.__authDialogShown = true;
    showAuthDialog();
  }
}

function buildMockError(status, body) {
  const error = new Error(body?.error || `mock request failed with status ${status}`);
  error.response = {
    status,
    data: body,
  };
  return error;
}

function toSearchParams(params = {}) {
  const searchParams = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value === undefined || value === null || value === "") return;
    searchParams.set(key, String(value));
  });
  return searchParams;
}

async function mockRequest(method, path, options = {}) {
  const response = mockSession.request({
    method,
    path,
    query: toSearchParams(options.params),
    body: options.data,
  });
  if (response.status >= 400) {
    if (response.status === 401) {
      handleUnauthorized();
    }
    throw buildMockError(response.status, response.body);
  }
  return response.body;
}

/**
 * Show authentication required dialog.
 */
function showAuthDialog() {
  window.dispatchEvent(new CustomEvent("auth-required"));
}

export async function getSummary() {
  if (useMockAPI) {
    const dashboard = await mockRequest("GET", "/api/dashboard");
    return dashboard.summary;
  }
  const { data } = await http.get("/api/summary");
  return data;
}

export async function getDashboard(params = {}) {
  if (useMockAPI) {
    return mockRequest("GET", "/api/dashboard", { params });
  }
  const { data } = await http.get("/api/dashboard", { params });
  return data;
}

export async function listWorkers() {
  if (useMockAPI) {
    return mockRequest("GET", "/api/workers");
  }
  const { data } = await http.get("/api/workers");
  return data;
}

export async function getClusterOverview() {
  if (useMockAPI) {
    return mockRequest("GET", "/api/cluster/overview");
  }
  const { data } = await http.get("/api/cluster/overview");
  return data;
}

export async function lookupSource(params = {}) {
  if (useMockAPI) {
    return mockRequest("GET", "/api/sources/lookup", { params });
  }
  const { data } = await http.get("/api/sources/lookup", { params });
  return data;
}

export async function listTasks(params = {}) {
  if (useMockAPI) {
    return mockRequest("GET", "/api/tasks", { params });
  }
  const { data } = await http.get("/api/tasks", { params });
  return data;
}

export async function getTask(id) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}`);
  }
  const { data } = await http.get(`/api/tasks/${id}`);
  return data;
}

export async function createTask(payload) {
  if (useMockAPI) {
    return mockRequest("POST", "/api/tasks", { data: payload });
  }
  const { data } = await http.post("/api/tasks", payload);
  return data;
}

export async function updateTask(id, payload) {
  if (useMockAPI) {
    return mockRequest("PUT", `/api/tasks/${id}`, { data: payload });
  }
  const { data } = await http.put(`/api/tasks/${id}`, payload);
  return data;
}

export async function deleteTask(id) {
  if (useMockAPI) {
    await mockRequest("DELETE", `/api/tasks/${id}`);
    return;
  }
  await http.delete(`/api/tasks/${id}`);
}

export async function startTask(id) {
  if (useMockAPI) {
    await mockRequest("POST", `/api/tasks/${id}/start`);
    return;
  }
  await http.post(`/api/tasks/${id}/start`);
}

export async function stopTask(id) {
  if (useMockAPI) {
    await mockRequest("POST", `/api/tasks/${id}/stop`);
    return;
  }
  await http.post(`/api/tasks/${id}/stop`);
}

export async function getCheckpoint(id) {
  try {
    if (useMockAPI) {
      return await mockRequest("GET", `/api/tasks/${id}/checkpoint`);
    }
    const { data } = await http.get(`/api/tasks/${id}/checkpoint`);
    return data;
  } catch (err) {
    if (err?.response?.status === 404) return null;
    throw err;
  }
}

export async function getReplication(id) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}/replication`);
  }
  const { data } = await http.get(`/api/tasks/${id}/replication`);
  return data;
}

export async function getTaskLease(id) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}/lease`);
  }
  const { data } = await http.get(`/api/tasks/${id}/lease`);
  return data;
}

export async function listTaskRuns(id, limit = 10) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}/runs`, {
      params: { limit },
    });
  }
  const { data } = await http.get(`/api/tasks/${id}/runs`, {
    params: { limit },
  });
  return data;
}

export async function listEvents(id, limit = 120) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}/events`, {
      params: { limit },
    });
  }
  const { data } = await http.get(`/api/tasks/${id}/events`, {
    params: { limit },
  });
  return data;
}

export async function listFiles(id, limit = 80) {
  if (useMockAPI) {
    return mockRequest("GET", `/api/tasks/${id}/files`, {
      params: { limit },
    });
  }
  const { data } = await http.get(`/api/tasks/${id}/files`, {
    params: { limit },
  });
  return data;
}

export async function retryUpload(id, limit = 100) {
  if (useMockAPI) {
    return mockRequest("POST", `/api/tasks/${id}/files/retry-upload`, {
      params: { limit },
    });
  }
  const { data } = await http.post(
    `/api/tasks/${id}/files/retry-upload`,
    null,
    {
      params: { limit },
    },
  );
  return data;
}
