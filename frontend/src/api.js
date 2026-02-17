import axios from "axios";

const http = axios.create({
  baseURL: "/",
  timeout: 10000,
});

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
  const { data } = await http.get(`/api/tasks/${id}/runs`, { params: { limit } });
  return data;
}

export async function listEvents(id, limit = 120) {
  const { data } = await http.get(`/api/tasks/${id}/events`, { params: { limit } });
  return data;
}

export async function listFiles(id, limit = 80) {
  const { data } = await http.get(`/api/tasks/${id}/files`, { params: { limit } });
  return data;
}
