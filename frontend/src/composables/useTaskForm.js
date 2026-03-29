// input: refreshAll callback, validateTaskPayload, API calls
// output: form state, form actions (open/submit/edit/start/stop/delete)
// pos: task CRUD form logic
import { reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage, ElMessageBox } from "element-plus";
import { createTask, updateTask, startTask, stopTask, deleteTask } from "../api.js";

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

export function useTaskForm({ refreshAll, validateTaskPayload, parseErr }) {
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
      await ElMessageBox.confirm(t("msg.confirmDelete", { id: task.id }), t("msg.deleteConfirmTitle"), { type: "warning" });
      await deleteTask(task.id);
      ElMessage.success(t("msg.taskDeleted", { id: task.id }));
      await refreshAll();
    } catch (err) {
      if (err !== "cancel") ElMessage.error(parseErr(err));
    }
  }

  return { formVisible, formMode, form, openCreate, openEdit, submitForm, onStart, onStop, onDelete };
}
