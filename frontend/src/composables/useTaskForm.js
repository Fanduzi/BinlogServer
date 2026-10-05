// input: refreshAll callback, parseErr, API calls
// output: form state and actions for create, catalog edit, leftover adopt, start, stop, and delete
// pos: task form logic; a leftover directory submits POST adopt and does not start replication
// note: if this file changes, update this header and frontend/src/composables/README.md.
import { reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage, ElMessageBox } from "element-plus";
import {
  adoptTask,
  createTask,
  updateTask,
  startTask,
  stopTask,
  deleteTask,
} from "../api.js";

// Form-only value. Omitted from POST /adopt so the server keeps FILE_POS at the highest segment.
export const ADOPT_START_DEFAULT = "DISK_END";

export function isLeftoverDiskTask(task) {
  const source = task?.source || {};
  return (
    String(source.host || "").trim() === "" &&
    String(source.user || "").trim() === "" &&
    String(task?.cluster_key || "").trim() === ""
  );
}

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
    storage: { retention_days: 7, local_retention_days: 0, bucket_retention_days: 0 },
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

  return validateStorage(t, payload?.storage);
}

function storagePayload(storage) {
  const out = { retention_days: Number(storage?.retention_days) };
  const local = Number(storage?.local_retention_days || 0);
  const bucket = Number(storage?.bucket_retention_days || 0);
  if (Number.isInteger(local) && local > 0) out.local_retention_days = local;
  if (Number.isInteger(bucket) && bucket > 0) out.bucket_retention_days = bucket;
  return out;
}

function optionalRetentionDays(value) {
  const days = Number(value || 0);
  if (!Number.isInteger(days) || days < 0 || days > RETENTION_DAYS_MAX) return false;
  if (days !== 0 && days < RETENTION_DAYS_MIN) return false;
  return true;
}

function validateStorage(t, storage) {
  const retentionDays = Number(storage?.retention_days || 0);
  if (
    !Number.isInteger(retentionDays) ||
    retentionDays < RETENTION_DAYS_MIN ||
    retentionDays > RETENTION_DAYS_MAX
  ) {
    return t("validation.retentionInvalid");
  }
  if (!optionalRetentionDays(storage?.local_retention_days)) {
    return t("validation.localRetentionInvalid");
  }
  if (!optionalRetentionDays(storage?.bucket_retention_days)) {
    return t("validation.bucketRetentionInvalid");
  }
  const local = Number(storage?.local_retention_days || 0) || retentionDays;
  const bucket = Number(storage?.bucket_retention_days || 0) || retentionDays;
  if (bucket < local) return t("validation.bucketRetentionShorter");
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
    if (isLeftoverDiskTask(task)) {
      openAdopt(task);
      return;
    }
    formMode.value = "edit";
    Object.assign(form, defaultForm(), JSON.parse(JSON.stringify(task)));
    form.source.password = "";
    formVisible.value = true;
  }

  function openAdopt(task) {
    formMode.value = "adopt";
    form.id = String(task?.id || "");
    form.name = String(task?.name || task?.id || "");
    form.cluster_key = "";
    form.source.host = "";
    form.source.port = 3306;
    form.source.user = "";
    form.source.password = "";
    form.source.flavor = "mysql";
    form.source.server_id = 200001;
    form.source.semi_sync = false;
    form.start.mode = ADOPT_START_DEFAULT;
    form.start.file = "";
    form.start.pos = 0;
    form.start.gtid_set = "";
    const retention = Number(task?.storage?.retention_days || 0);
    form.storage.retention_days =
      Number.isInteger(retention) && retention >= RETENTION_DAYS_MIN && retention <= RETENTION_DAYS_MAX
        ? retention
        : 7;
    form.storage.local_retention_days = Number(task?.storage?.local_retention_days || 0);
    form.storage.bucket_retention_days = Number(task?.storage?.bucket_retention_days || 0);
    formVisible.value = true;
  }

  function buildAdoptPayload() {
    const payload = {
      cluster_key: form.cluster_key?.trim(),
      source: {
        host: form.source.host?.trim() || "",
        port: Number(form.source.port || 0),
        user: form.source.user?.trim() || "",
        password: form.source.password,
        flavor: form.source.flavor?.trim() || "",
        server_id: Number(form.source.server_id || 0),
        semi_sync: !!form.source.semi_sync,
      },
    };
    const name = form.name.trim();
    if (name) payload.name = name;
    const mode = String(form.start.mode || "").trim();
    if (mode && mode !== ADOPT_START_DEFAULT) {
      payload.start = { mode };
      if (mode === "FILE_POS") {
        payload.start.file = form.start.file?.trim() || "";
        payload.start.pos = Number(form.start.pos || 0);
      }
      if (mode === "GTID") {
        payload.start.gtid_set = form.start.gtid_set?.trim() || "";
      }
    }
    const retentionDays = Number(form.storage.retention_days || 0);
    if (retentionDays) payload.storage = storagePayload(form.storage);
    return payload;
  }

  function validateAdoptPayload(payload) {
    const name = String(payload?.name || "").trim();
    if (name && name.length > NAME_MAX_LENGTH) return t("validation.taskNameInvalid");
    const clusterKeyErr = validateClusterKey(t, payload?.cluster_key);
    if (clusterKeyErr) return clusterKeyErr;

    const source = payload?.source || {};
    const host = String(source.host || "").trim();
    const user = String(source.user || "").trim();
    const flavor = String(source.flavor || "").trim() || "mysql";
    const port = Number(source.port || 0);
    const serverID = Number(source.server_id || 0);
    if (!host || host.length > SOURCE_HOST_MAX_LENGTH || hasWhitespace(host)) {
      return t("validation.hostInvalid");
    }
    if (!Number.isInteger(port) || port < 1 || port > 65535) return t("validation.portInvalid");
    if (!user || user.length > SOURCE_USER_MAX_LENGTH || hasWhitespace(user)) {
      return t("validation.userInvalid");
    }
    if (!String(source.password || "")) return t("validation.passwordRequired");
    if (!flavor || flavor.length > SOURCE_FLAVOR_MAX_LENGTH || !CLUSTER_KEY_PATTERN.test(flavor)) {
      return t("validation.flavorInvalid");
    }
    if (!Number.isInteger(serverID) || serverID < 0 || serverID > 4294967295) {
      return t("validation.serverIdInvalid");
    }

    const mode = String(payload?.start?.mode || "").trim();
    if (mode) {
      if (!["LATEST", "FILE_POS", "GTID"].includes(mode)) return t("validation.startModeInvalid");
      if (mode === "FILE_POS") {
        const file = String(payload.start.file || "").trim();
        const pos = Number(payload.start.pos || 0);
        if (!file || file.length > START_FILE_MAX_LENGTH || !Number.isInteger(pos) || pos <= 0) {
          return t("validation.filePosRequired");
        }
      }
      if (mode === "GTID" && !String(payload.start.gtid_set || "").trim()) {
        return t("validation.gtidSetRequired");
      }
    }

    if (payload?.storage) {
      const storageErr = validateStorage(t, payload.storage);
      if (storageErr) return storageErr;
    }
    return "";
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
      storage: storagePayload(form.storage),
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
      const adopting = formMode.value === "adopt";
      const payload = adopting ? buildAdoptPayload() : buildPayload();
      const validationErr = adopting ? validateAdoptPayload(payload) : validateTaskPayload(payload);
      if (validationErr) {
        ElMessage.error(validationErr);
        return;
      }
      if (formMode.value === "create") {
        await createTask(payload);
        ElMessage.success(t("msg.taskCreated"));
      } else if (adopting) {
        await adoptTask(form.id, payload);
        form.source.password = "";
        ElMessage.success(t("msg.taskAdopted"));
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
    openAdopt,
    buildPayload,
    buildAdoptPayload,
    validateTaskPayload,
    validateAdoptPayload,
    resetForm,
    submitForm,
    onStart,
    onStop,
    onDelete,
  };
}
