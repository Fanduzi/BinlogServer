// input: refreshAll callback, parseErr, API calls
// output: form state, form actions (open/submit/edit/start/stop/delete)
// pos: task CRUD form logic
import { reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  createTask,
  updateTask,
  startTask,
  stopTask,
  deleteTask,
} from "../api.js";

const NAME_MAX_LENGTH = 255;
const CLUSTER_KEY_PATTERN = /^[A-Za-z0-9._-]+$/;
const SOURCE_HOST_MAX_LENGTH = 255;
const SOURCE_USER_MAX_LENGTH = 128;
const SOURCE_FLAVOR_MAX_LENGTH = 32;
const START_FILE_MAX_LENGTH = 255;
const RETENTION_DAYS_MIN = 1;
const RETENTION_DAYS_MAX = 3650;

function hasWhitespace(text) {
  return /\s/.test(String(text || ""));
}

function defaultForm() {
  return {
    id: "",
    name: "",
    cluster_key: "",
    source: {
      host: "127.0.0.1",
      port: 3306,
      user: "repl",
      password: "",
      flavor: "mysql",
      server_id: 200001,
      semi_sync: false,
    },
    start: { mode: "LATEST", file: "", pos: 0, gtid_set: "" },
    storage: { retention_days: 7 },
  };
}

function validateClusterKey(t, clusterKeyRaw) {
  const clusterKey = String(clusterKeyRaw || "").trim();
  if (!clusterKey) {
    return t("validation.clusterKeyEmpty");
  }
  if (
    clusterKey.includes("/") ||
    clusterKey.includes("\\") ||
    clusterKey.includes("..") ||
    !CLUSTER_KEY_PATTERN.test(clusterKey)
  ) {
    return t("validation.clusterKeyInvalid");
  }
  return "";
}

function validateTaskPayloadFn(t, payload) {
  const name = String(payload?.name || "").trim();
  if (!name || name.length > NAME_MAX_LENGTH) {
    return t("validation.taskNameInvalid");
  }
  const clusterKeyErr = validateClusterKey(t, payload?.cluster_key);
  if (clusterKeyErr) return clusterKeyErr;

  const source = payload?.source || {};
  const host = String(source.host || "").trim();
  const user = String(source.user || "").trim();
  const flavorRaw = String(source.flavor || "").trim();
  const flavor = flavorRaw || "mysql";
  const port = Number(source.port || 0);
  const serverID = Number(source.server_id || 0);

  if (!host || host.length > SOURCE_HOST_MAX_LENGTH || hasWhitespace(host)) {
    return t("validation.hostInvalid");
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return t("validation.portInvalid");
  }
  if (!user || user.length > SOURCE_USER_MAX_LENGTH || hasWhitespace(user)) {
    return t("validation.userInvalid");
  }
  if (
    !flavor ||
    flavor.length > SOURCE_FLAVOR_MAX_LENGTH ||
    !CLUSTER_KEY_PATTERN.test(flavor)
  ) {
    return t("validation.flavorInvalid");
  }
  if (!Number.isInteger(serverID) || serverID < 0 || serverID > 4294967295) {
    return t("validation.serverIdInvalid");
  }

  const start = payload?.start || {};
  const mode = String(start.mode || "").trim();
  if (!["LATEST", "FILE_POS", "GTID"].includes(mode)) {
    return t("validation.startModeInvalid");
  }
  if (mode === "FILE_POS") {
    const file = String(start.file || "").trim();
    const pos = Number(start.pos || 0);
    if (
      !file ||
      file.length > START_FILE_MAX_LENGTH ||
      !Number.isInteger(pos) ||
      pos <= 0
    ) {
      return t("validation.filePosRequired");
    }
  }
  if (mode === "GTID") {
    const gtidSet = String(start.gtid_set || "").trim();
    if (!gtidSet) return t("validation.gtidSetRequired");
  }

  const retentionDays = Number(payload?.storage?.retention_days || 0);
  if (
    !Number.isInteger(retentionDays) ||
    retentionDays < RETENTION_DAYS_MIN ||
    retentionDays > RETENTION_DAYS_MAX
  ) {
    return t("validation.retentionInvalid");
  }
  return "";
}

export function useTaskForm({ refreshAll, parseErr }) {
  const { t } = useI18n();

  const formVisible = ref(false);
  const formMode = ref("create");
  const form = reactive(defaultForm());

  function resetForm() {
    Object.assign(form, defaultForm());
  }

  function openCreate() {
    formMode.value = "create";
    resetForm();
    formVisible.value = true;
  }

  function openEdit(task) {
    formMode.value = "edit";
    Object.assign(form, defaultForm(), JSON.parse(JSON.stringify(task)));
    form.source.password = "";
    formVisible.value = true;
  }

  function buildPayload() {
    const payload = {
      name: form.name.trim(),
      cluster_key: form.cluster_key?.trim(),
      source: {
        ...form.source,
        host: form.source.host?.trim() || "",
        user: form.source.user?.trim() || "",
        flavor: form.source.flavor?.trim() || "",
        semi_sync: !!form.source.semi_sync,
      },
      start: { mode: form.start.mode },
      storage: { retention_days: Number(form.storage.retention_days) },
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

  function validateTaskPayload(payload) {
    return validateTaskPayloadFn(t, payload);
  }

  async function submitForm() {
    try {
      const payload = buildPayload();
      const validationErr = validateTaskPayload(payload);
      if (validationErr) {
        ElMessage.error(validationErr);
        return;
      }
      if (formMode.value === "create") {
        await createTask(payload);
        ElMessage.success(t("msg.taskCreated"));
      } else {
        await updateTask(form.id, payload);
        ElMessage.success(t("msg.taskUpdated"));
      }
      formVisible.value = false;
      await refreshAll();
    } catch (err) {
      ElMessage.error(parseErr(err));
    }
  }

  async function onStart(task) {
    try {
      await startTask(task.id);
      ElMessage.success(t("msg.taskStarted", { id: task.id }));
      await refreshAll();
    } catch (err) {
      ElMessage.error(parseErr(err));
    }
  }

  async function onStop(task) {
    try {
      await stopTask(task.id);
      ElMessage.success(t("msg.taskStopped", { id: task.id }));
      await refreshAll();
    } catch (err) {
      ElMessage.error(parseErr(err));
    }
  }

  async function onDelete(task) {
    try {
      await ElMessageBox.confirm(
        t("msg.confirmDelete", { id: task.id }),
        t("msg.deleteConfirmTitle"),
        { type: "warning" },
      );
      await deleteTask(task.id);
      ElMessage.success(t("msg.taskDeleted", { id: task.id }));
      await refreshAll();
    } catch (err) {
      if (err !== "cancel") ElMessage.error(parseErr(err));
    }
  }

  return {
    formVisible,
    formMode,
    form,
    openCreate,
    openEdit,
    buildPayload,
    validateTaskPayload,
    resetForm,
    submitForm,
    onStart,
    onStop,
    onDelete,
  };
}
