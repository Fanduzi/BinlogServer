// input: axios HTTP client, utils/auth.js token storage, backend 401 responses
// output: API request helpers plus auth-required event dispatch for the settings flow
// pos: frontend API layer with auth interceptors for backend communication
// note: keep 401 handling aligned with in-app settings guidance; update frontend/README.md if responsibilities change

import axios from "axios";
import { getAuthToken, clearAuthToken } from "./utils/auth.js";

const http = axios.create({
  baseURL: "/",
  timeout: 10000,
});

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
      // Clear invalid token.
      clearAuthToken();
      // Show auth dialog if not already shown.
      if (!window.__authDialogShown) {
        window.__authDialogShown = true;
        showAuthDialog();
      }
    }
    return Promise.reject(error);
  },
);

/**
 * Show authentication required dialog.
 */
function showAuthDialog() {
  const messageLines = [
    "接口认证已失效或尚未配置。",
    "请按以下步骤处理：",
    "1. 向管理员获取 API Token",
    "2. 打开“设置”并填写 Token",
    "3. 保存后重新刷新数据",
  ];

  window.dispatchEvent(
    new CustomEvent("auth-required", {
      detail: {
        message: messageLines.join("\n"),
        lines: messageLines,
      },
    }),
  );
}

export async function getSummary() {
  const { data } = await http.get("/api/summary");
  return data;
}

export async function getDashboard(params = {}) {
  const { data } = await http.get("/api/dashboard", { params });
  return data;
}

export async function listWorkers() {
  const { data } = await http.get("/api/workers");
  return data;
}

export async function getClusterOverview() {
  const { data } = await http.get("/api/cluster/overview");
  return data;
}

export async function lookupSource(params = {}) {
  const { data } = await http.get("/api/sources/lookup", { params });
  return data;
}

export async function listTasks(params = {}) {
  const { data } = await http.get("/api/tasks", { params });
  return data;
}

export async function getTask(id) {
  const { data } = await http.get(`/api/tasks/${id}`);
  return data;
}

export async function createTask(payload) {
  const { data } = await http.post("/api/tasks", payload);
  return data;
}

export async function updateTask(id, payload) {
  const { data } = await http.put(`/api/tasks/${id}`, payload);
  return data;
}

export async function deleteTask(id) {
  await http.delete(`/api/tasks/${id}`);
}

export async function startTask(id) {
  await http.post(`/api/tasks/${id}/start`);
}

export async function stopTask(id) {
  await http.post(`/api/tasks/${id}/stop`);
}

export async function getCheckpoint(id) {
  try {
    const { data } = await http.get(`/api/tasks/${id}/checkpoint`);
    return data;
  } catch (err) {
    if (err?.response?.status === 404) return null;
    throw err;
  }
}

export async function getReplication(id) {
  const { data } = await http.get(`/api/tasks/${id}/replication`);
  return data;
}

export async function getTaskLease(id) {
  const { data } = await http.get(`/api/tasks/${id}/lease`);
  return data;
}

export async function listTaskRuns(id, limit = 10) {
  const { data } = await http.get(`/api/tasks/${id}/runs`, {
    params: { limit },
  });
  return data;
}

export async function listEvents(id, limit = 120) {
  const { data } = await http.get(`/api/tasks/${id}/events`, {
    params: { limit },
  });
  return data;
}

export async function listFiles(id, limit = 80) {
  const { data } = await http.get(`/api/tasks/${id}/files`, {
    params: { limit },
  });
  return data;
}

export async function retryUpload(id, limit = 100) {
  const { data } = await http.post(
    `/api/tasks/${id}/files/retry-upload`,
    null,
    {
      params: { limit },
    },
  );
  return data;
}
