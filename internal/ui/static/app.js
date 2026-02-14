const state = {
  tasks: [],
  editingId: "",
  selectedId: "",
};

const formEl = document.getElementById("taskForm");
const formTitleEl = document.getElementById("formTitle");
const taskTableEl = document.getElementById("taskTable");
const taskCountEl = document.getElementById("taskCount");
const detailHintEl = document.getElementById("detailHint");
const detailContentEl = document.getElementById("detailContent");
const sumTotalEl = document.getElementById("sumTotal");
const sumRunningEl = document.getElementById("sumRunning");
const sumRetryEl = document.getElementById("sumRetry");
const sumStoppedEl = document.getElementById("sumStopped");
const sumFailedEl = document.getElementById("sumFailed");

document.getElementById("refreshBtn").addEventListener("click", () => loadTasks(true));
document.getElementById("cancelEditBtn").addEventListener("click", resetForm);
formEl.addEventListener("submit", onSubmitForm);

loadTasks(false);
setInterval(() => {
  loadTasks(true).catch(() => {});
}, 15000);

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || `request failed: ${res.status}`);
  }
  if (res.status === 204) return null;
  return await res.json();
}

function readForm() {
  const fd = new FormData(formEl);
  const mode = fd.get("start_mode");
  const data = {
    name: String(fd.get("name") || "").trim(),
    source: {
      host: String(fd.get("host") || "").trim(),
      port: Number(fd.get("port") || 3306),
      user: String(fd.get("user") || "").trim(),
      password: String(fd.get("password") || ""),
      flavor: String(fd.get("flavor") || "mysql"),
      server_id: Number(fd.get("server_id") || 0),
    },
    start: { mode },
    storage: {
      retention_days: Number(fd.get("retention_days") || 7),
    },
  };

  if (mode === "FILE_POS") {
    data.start.file = String(fd.get("start_file") || "").trim();
    data.start.pos = Number(fd.get("start_pos") || 0);
  } else if (mode === "GTID") {
    data.start.gtid_set = String(fd.get("start_gtid") || "").trim();
  }
  return data;
}

function fillForm(task) {
  formEl.name.value = task.name || "";
  formEl.host.value = task.source?.host || "";
  formEl.port.value = task.source?.port || 3306;
  formEl.user.value = task.source?.user || "";
  formEl.password.value = task.source?.password || "";
  formEl.server_id.value = task.source?.server_id || 200001;
  formEl.flavor.value = task.source?.flavor || "mysql";
  formEl.retention_days.value = task.storage?.retention_days || 7;
  formEl.start_mode.value = task.start?.mode || "LATEST";
  formEl.start_file.value = task.start?.file || "";
  formEl.start_pos.value = task.start?.pos || "";
  formEl.start_gtid.value = task.start?.gtid_set || "";
}

function resetForm() {
  state.editingId = "";
  formTitleEl.textContent = "创建任务";
  formEl.reset();
  formEl.port.value = 3306;
  formEl.server_id.value = 200001;
  formEl.retention_days.value = 7;
  formEl.start_mode.value = "LATEST";
}

async function onSubmitForm(e) {
  e.preventDefault();
  try {
    const payload = readForm();
    if (!payload.name) throw new Error("任务名不能为空");

    if (state.editingId) {
      await api(`/api/tasks/${state.editingId}`, {
        method: "PUT",
        body: JSON.stringify(payload),
      });
    } else {
      await api("/api/tasks", {
        method: "POST",
        body: JSON.stringify(payload),
      });
    }
    resetForm();
    await loadTasks(true);
  } catch (err) {
    alert(err.message || String(err));
  }
}

async function loadTasks(keepSelection) {
  try {
    state.tasks = await api("/api/tasks");
    await loadSummary();
    renderTaskTable();
    taskCountEl.textContent = `${state.tasks.length} tasks`;

    if (keepSelection && state.selectedId) {
      await showDetail(state.selectedId);
    } else if (!state.selectedId && state.tasks.length > 0) {
      await showDetail(state.tasks[0].id);
    }
  } catch (err) {
    taskTableEl.innerHTML = `<p class="muted">加载失败: ${escapeHtml(err.message || String(err))}</p>`;
  }
}

async function loadSummary() {
  const summary = await api("/api/summary");
  sumTotalEl.textContent = String(summary.total || 0);
  sumRunningEl.textContent = String(summary.running || 0);
  sumRetryEl.textContent = String(summary.retry_backoff || 0);
  sumStoppedEl.textContent = String(summary.stopped || 0);
  sumFailedEl.textContent = String(summary.failed || 0);
}

function renderTaskTable() {
  if (state.tasks.length === 0) {
    taskTableEl.innerHTML = `<p class="muted">暂无任务，先在左侧创建。</p>`;
    return;
  }

  const rows = state.tasks.map((t) => {
    const src = `${t.source?.host || "-"}:${t.source?.port || "-"}`;
    const start = t.start?.mode || "-";
    const retention = t.storage?.retention_days || "-";
    const stateBadge = `<span class="state">${escapeHtml(t.state || "-")}</span>`;

    return `
      <tr data-id="${escapeHtml(t.id)}">
        <td>${escapeHtml(t.id)}</td>
        <td>${escapeHtml(t.name || "-")}</td>
        <td>${stateBadge}</td>
        <td>${escapeHtml(src)}</td>
        <td>${escapeHtml(start)}</td>
        <td>${escapeHtml(String(retention))}</td>
        <td>${escapeHtml(t.last_error || "-")}</td>
        <td>
          <div class="row-actions">
            <button class="mini" data-op="view" data-id="${escapeHtml(t.id)}">详情</button>
            <button class="mini" data-op="edit" data-id="${escapeHtml(t.id)}">编辑</button>
            <button class="mini" data-op="start" data-id="${escapeHtml(t.id)}">启动</button>
            <button class="mini" data-op="stop" data-id="${escapeHtml(t.id)}">停止</button>
            <button class="mini" data-op="delete" data-id="${escapeHtml(t.id)}">删除</button>
          </div>
        </td>
      </tr>
    `;
  }).join("");

  taskTableEl.innerHTML = `
    <table>
      <thead>
        <tr>
          <th>ID</th><th>名称</th><th>状态</th><th>源库</th><th>起点</th><th>保留</th><th>错误</th><th>操作</th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
    </table>
  `;

  taskTableEl.querySelectorAll("button[data-op]").forEach((btn) => {
    btn.addEventListener("click", onRowAction);
  });
}

async function onRowAction(e) {
  const op = e.currentTarget.dataset.op;
  const id = e.currentTarget.dataset.id;
  try {
    if (op === "view") {
      await showDetail(id);
      return;
    }
    if (op === "edit") {
      const task = await api(`/api/tasks/${id}`);
      state.editingId = id;
      formTitleEl.textContent = `编辑任务 #${id}`;
      fillForm(task);
      return;
    }
    if (op === "start") {
      await api(`/api/tasks/${id}/start`, { method: "POST" });
    } else if (op === "stop") {
      await api(`/api/tasks/${id}/stop`, { method: "POST" });
    } else if (op === "delete") {
      if (!confirm(`确认删除任务 #${id} ?`)) return;
      await api(`/api/tasks/${id}`, { method: "DELETE" });
      if (state.selectedId === id) {
        state.selectedId = "";
        detailContentEl.innerHTML = "";
      }
    }
    await loadTasks(true);
  } catch (err) {
    alert(err.message || String(err));
  }
}

async function showDetail(id) {
  state.selectedId = id;
  detailHintEl.textContent = `任务 #${id}`;

  const [task, checkpoint, events, files] = await Promise.all([
    api(`/api/tasks/${id}`),
    fetch(`/api/tasks/${id}/checkpoint`).then(async (res) => {
      if (res.status === 404) return null;
      if (!res.ok) throw new Error(await res.text());
      return await res.json();
    }),
    api(`/api/tasks/${id}/events?limit=120`),
    api(`/api/tasks/${id}/files?limit=80`),
  ]);

  const cpBlock = checkpoint
    ? `<p><strong>${escapeHtml(checkpoint.file)}:${escapeHtml(String(checkpoint.pos))}</strong></p>`
    : `<p class="muted">暂无 checkpoint</p>`;

  const eventHtml = (events || []).map((ev) => `
    <div class="event-item">
      <div><strong>${escapeHtml(ev.type || "-")}</strong></div>
      <div>${escapeHtml(ev.message || "")}</div>
      <div class="muted">${escapeHtml(ev.time || "")}</div>
    </div>
  `).join("");

  const filesHtml = (files || []).map((f) => `
    <tr>
      <td>${escapeHtml(f.file_name || "-")}</td>
      <td>${escapeHtml(String(f.size_bytes || 0))}</td>
      <td>${escapeHtml(String(f.start_pos || 0))}</td>
      <td>${escapeHtml(String(f.end_pos || 0))}</td>
      <td>${escapeHtml(f.sealed_at || "")}</td>
    </tr>
  `).join("");

  detailContentEl.innerHTML = `
    <section class="card">
      <h3>${escapeHtml(task.name || "-")}</h3>
      <p class="muted">${escapeHtml(task.state || "-")}</p>
      <p>源库: ${escapeHtml((task.source?.host || "-") + ":" + (task.source?.port || "-"))}</p>
      <p>起点: ${escapeHtml(task.start?.mode || "-")}</p>
      <p>保留: ${escapeHtml(String(task.storage?.retention_days || "-"))} 天</p>
    </section>
    <section class="card">
      <h3>Checkpoint</h3>
      ${cpBlock}
      <h3>Files</h3>
      ${filesHtml ? `
        <div class="events">
          <table>
            <thead><tr><th>File</th><th>Size</th><th>Start</th><th>End</th><th>SealedAt</th></tr></thead>
            <tbody>${filesHtml}</tbody>
          </table>
        </div>
      ` : '<p class="muted">暂无落盘文件记录</p>'}
      <h3>Events</h3>
      <div class="events">${eventHtml || '<p class="muted">暂无事件</p>'}</div>
    </section>
  `;
}

function escapeHtml(input) {
  return String(input)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}
