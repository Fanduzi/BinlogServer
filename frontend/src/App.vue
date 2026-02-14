<template>
  <div class="page">
    <header class="hero">
      <div>
        <p class="eyebrow">Binlog Backup Console</p>
        <h1>Binlog Server 管理台</h1>
      </div>
      <div class="actions">
        <el-button type="primary" @click="openCreate">新建任务</el-button>
        <el-button @click="refreshAll">刷新</el-button>
      </div>
    </header>

    <section class="summary">
      <div class="card">
        <span>总任务</span>
        <strong>{{ summary.total }}</strong>
      </div>
      <div class="card">
        <span>运行中</span>
        <strong>{{ summary.running }}</strong>
      </div>
      <div class="card">
        <span>重试中</span>
        <strong>{{ summary.retry_backoff }}</strong>
      </div>
      <div class="card">
        <span>已停止</span>
        <strong>{{ summary.stopped }}</strong>
      </div>
      <div class="card">
        <span>失败</span>
        <strong>{{ summary.failed }}</strong>
      </div>
    </section>

    <el-card shadow="never" class="list-card">
      <template #header>
        <div class="card-header">
          <span>任务列表</span>
          <span class="muted">{{ tasks.length }} tasks</span>
        </div>
      </template>

      <el-table :data="tasks" stripe border @row-click="showDetail">
        <el-table-column prop="id" label="ID" width="70" />
        <el-table-column prop="name" label="名称" min-width="150" />
        <el-table-column label="状态" width="130">
          <template #default="{ row }">
            <el-tag size="small" :type="stateTagType(row.state)">{{ row.state }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="源库" min-width="190">
          <template #default="{ row }">
            {{ row.source?.host || "-" }}:{{ row.source?.port || "-" }}
          </template>
        </el-table-column>
        <el-table-column label="起点" width="110">
          <template #default="{ row }">{{ row.start?.mode || "-" }}</template>
        </el-table-column>
        <el-table-column label="保留天数" width="100">
          <template #default="{ row }">{{ row.storage?.retention_days || "-" }}</template>
        </el-table-column>
        <el-table-column label="操作" width="280" fixed="right">
          <template #default="{ row }">
            <el-space wrap>
              <el-button size="small" @click.stop="showDetail(row)">详情</el-button>
              <el-button size="small" @click.stop="openEdit(row)">编辑</el-button>
              <el-button size="small" type="success" @click.stop="onStart(row)">启动</el-button>
              <el-button size="small" type="warning" @click.stop="onStop(row)">停止</el-button>
              <el-button size="small" type="danger" @click.stop="onDelete(row)">删除</el-button>
            </el-space>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="formVisible" :title="formMode === 'create' ? '新建任务' : `编辑任务 #${form.id}`" width="860px">
      <el-form :model="form" label-width="92px">
        <el-row :gutter="12">
          <el-col :span="12"><el-form-item label="任务名"><el-input v-model="form.name" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="主机"><el-input v-model="form.source.host" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="端口"><el-input-number v-model="form.source.port" :min="1" :max="65535" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="用户"><el-input v-model="form.source.user" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="密码"><el-input v-model="form.source.password" show-password placeholder="编辑时留空=不修改" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="Server ID"><el-input-number v-model="form.source.server_id" :min="1" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="Flavor"><el-input v-model="form.source.flavor" /></el-form-item></el-col>
          <el-col :span="12"><el-form-item label="保留天数"><el-input-number v-model="form.storage.retention_days" :min="1" /></el-form-item></el-col>
          <el-col :span="8">
            <el-form-item label="起点模式">
              <el-select v-model="form.start.mode" style="width: 100%">
                <el-option value="LATEST" label="LATEST" />
                <el-option value="FILE_POS" label="FILE_POS" />
                <el-option value="GTID" label="GTID" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="8"><el-form-item label="文件"><el-input v-model="form.start.file" /></el-form-item></el-col>
          <el-col :span="8"><el-form-item label="位置"><el-input-number v-model="form.start.pos" :min="0" /></el-form-item></el-col>
          <el-col :span="24"><el-form-item label="GTID"><el-input v-model="form.start.gtid_set" /></el-form-item></el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="formVisible = false">取消</el-button>
        <el-button type="primary" @click="submitForm">保存</el-button>
      </template>
    </el-dialog>

    <el-drawer v-model="detailVisible" size="65%" :title="detailTask ? `任务详情 #${detailTask.id}` : '任务详情'">
      <template v-if="detailTask">
        <el-descriptions border :column="2">
          <el-descriptions-item label="名称">{{ detailTask.name }}</el-descriptions-item>
          <el-descriptions-item label="状态">{{ detailTask.state }}</el-descriptions-item>
          <el-descriptions-item label="源库">{{ detailTask.source?.host }}:{{ detailTask.source?.port }}</el-descriptions-item>
          <el-descriptions-item label="起点">{{ detailTask.start?.mode }}</el-descriptions-item>
        </el-descriptions>

        <el-divider content-position="left">Checkpoint</el-divider>
        <div class="checkpoint">{{ checkpoint ? `${checkpoint.file}:${checkpoint.pos}` : "暂无" }}</div>

        <el-divider content-position="left">文件元数据</el-divider>
        <el-table :data="files" size="small" border>
          <el-table-column prop="file_name" label="File" min-width="180" />
          <el-table-column prop="size_bytes" label="Size" width="100" />
          <el-table-column prop="start_pos" label="Start" width="90" />
          <el-table-column prop="end_pos" label="End" width="90" />
          <el-table-column prop="upload_state" label="Upload" width="130" />
          <el-table-column prop="object_key" label="ObjectKey" min-width="190" />
        </el-table>

        <el-divider content-position="left">事件</el-divider>
        <el-timeline>
          <el-timeline-item v-for="ev in events" :key="`${ev.sequence}-${ev.type}`" :timestamp="ev.time">
            <strong>{{ ev.type }}</strong>
            <p>{{ ev.message }}</p>
          </el-timeline-item>
        </el-timeline>
      </template>
    </el-drawer>
  </div>
</template>

<script setup>
import { reactive, ref } from "vue";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  createTask,
  deleteTask,
  getCheckpoint,
  getSummary,
  getTask,
  listEvents,
  listFiles,
  listTasks,
  startTask,
  stopTask,
  updateTask,
} from "./api";

const summary = reactive({ total: 0, running: 0, retry_backoff: 0, stopped: 0, failed: 0 });
const tasks = ref([]);

const formVisible = ref(false);
const formMode = ref("create");
const form = reactive(defaultForm());

const detailVisible = ref(false);
const detailTask = ref(null);
const checkpoint = ref(null);
const events = ref([]);
const files = ref([]);

refreshAll();

async function refreshAll() {
  try {
    const [sum, list] = await Promise.all([getSummary(), listTasks()]);
    Object.assign(summary, sum || {});
    tasks.value = list || [];
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

function defaultForm() {
  return {
    id: "",
    name: "",
    source: {
      host: "127.0.0.1",
      port: 3306,
      user: "repl",
      password: "",
      flavor: "mysql",
      server_id: 200001,
    },
    start: {
      mode: "LATEST",
      file: "",
      pos: 0,
      gtid_set: "",
    },
    storage: {
      retention_days: 7,
    },
  };
}

function resetForm() {
  Object.assign(form, defaultForm());
}

function openCreate() {
  formMode.value = "create";
  resetForm();
  formVisible.value = true;
}

function openEdit(row) {
  formMode.value = "edit";
  Object.assign(form, defaultForm(), JSON.parse(JSON.stringify(row)));
  form.source.password = "";
  formVisible.value = true;
}

function buildPayload() {
  const payload = {
    name: form.name.trim(),
    source: { ...form.source },
    start: { mode: form.start.mode },
    storage: { retention_days: Number(form.storage.retention_days || 7) },
  };
  if (!payload.source.password) delete payload.source.password;

  if (payload.start.mode === "FILE_POS") {
    payload.start.file = form.start.file?.trim() || "";
    payload.start.pos = Number(form.start.pos || 0);
  }
  if (payload.start.mode === "GTID") {
    payload.start.gtid_set = form.start.gtid_set?.trim() || "";
  }
  return payload;
}

async function submitForm() {
  try {
    const payload = buildPayload();
    if (!payload.name) {
      ElMessage.error("任务名不能为空");
      return;
    }
    if (formMode.value === "create") {
      await createTask(payload);
      ElMessage.success("任务已创建");
    } else {
      await updateTask(form.id, payload);
      ElMessage.success("任务已更新");
    }
    formVisible.value = false;
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onStart(row) {
  try {
    await startTask(row.id);
    ElMessage.success(`任务 #${row.id} 已启动`);
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onStop(row) {
  try {
    await stopTask(row.id);
    ElMessage.success(`任务 #${row.id} 已停止`);
    await refreshAll();
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

async function onDelete(row) {
  try {
    await ElMessageBox.confirm(`确认删除任务 #${row.id} ?`, "删除确认", { type: "warning" });
    await deleteTask(row.id);
    ElMessage.success(`任务 #${row.id} 已删除`);
    await refreshAll();
  } catch (err) {
    if (err !== "cancel") ElMessage.error(parseErr(err));
  }
}

async function showDetail(row) {
  try {
    const id = row.id || row;
    const [task, cp, evs, fs] = await Promise.all([
      getTask(id),
      getCheckpoint(id),
      listEvents(id, 120),
      listFiles(id, 80),
    ]);
    detailTask.value = task;
    checkpoint.value = cp;
    events.value = evs || [];
    files.value = fs || [];
    detailVisible.value = true;
  } catch (err) {
    ElMessage.error(parseErr(err));
  }
}

function stateTagType(state) {
  if (state === "RUNNING") return "success";
  if (state === "RETRY_BACKOFF") return "warning";
  if (state === "FAILED") return "danger";
  return "info";
}

function parseErr(err) {
  return err?.response?.data || err?.message || String(err);
}
</script>

<style scoped>
.page {
  max-width: 1440px;
  margin: 0 auto;
  padding: 20px;
  min-height: 100vh;
  background: linear-gradient(135deg, #eef5f8, #f9fcfd);
}

.hero {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  margin-bottom: 16px;
}

.eyebrow {
  margin: 0;
  font-size: 12px;
  color: #0f766e;
  letter-spacing: .1em;
  text-transform: uppercase;
}

h1 {
  margin: 6px 0 0;
  font-size: 34px;
}

.actions {
  display: flex;
  gap: 10px;
}

.summary {
  display: grid;
  grid-template-columns: repeat(5, minmax(120px, 1fr));
  gap: 12px;
  margin-bottom: 16px;
}

.card {
  background: #fff;
  border: 1px solid #dde8ee;
  border-radius: 12px;
  padding: 10px 12px;
}

.card span {
  color: #5f7281;
  font-size: 12px;
}

.card strong {
  display: block;
  margin-top: 6px;
  font-size: 26px;
}

.list-card {
  margin-bottom: 14px;
}

.card-header {
  display: flex;
  justify-content: space-between;
  width: 100%;
}

.muted {
  color: #6b7e8b;
}

.checkpoint {
  font-size: 14px;
  color: #334b59;
}

@media (max-width: 1080px) {
  .summary {
    grid-template-columns: repeat(2, minmax(120px, 1fr));
  }
  .hero {
    flex-direction: column;
    align-items: flex-start;
    gap: 10px;
  }
}
</style>
