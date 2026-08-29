// input: refreshAll callback, validateTaskPayload, parseErr, and batch/start API calls
// output: batchForm state, local 100-item preview validation, one-request batch creation actions, and safe text errors
// pos: batch task creation logic
// note: if this file changes, update this header and frontend/src/composables/README.md.
import { reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage, ElMessageBox } from "element-plus";
import { createTasksBatch, startTask } from "../api.js";

const MAX_BATCH_ITEMS = 100;

function defaultBatchForm() {
  return {
    lines: "",
    user: "",
    password: "",
    flavor: "mysql",
    serverIdStart: 300000,
    retentionDays: 7,
    semiSync: false,
    autoStart: false,
  };
}

export function useBatchCreate({ refreshAll, validateTaskPayload, parseErr }) {
  const { t } = useI18n();

  const batchVisible = ref(false);
  const batchForm = reactive(defaultBatchForm());
  const batchPreview = reactive({
    ready: false,
    canSubmit: false,
    validCount: 0,
    rows: [],
    errors: [],
  });

  function clearBatchPreview() {
    batchPreview.ready = false;
    batchPreview.canSubmit = false;
    batchPreview.validCount = 0;
    batchPreview.rows = [];
    batchPreview.errors = [];
  }

  function resetBatchForm() {
    Object.assign(batchForm, defaultBatchForm());
    clearBatchPreview();
  }

  function openBatchCreate() {
    resetBatchForm();
    batchVisible.value = true;
  }

  function makeClusterKey(name, host, port, lineNo = 0) {
    const raw = `${name}-${host}-${port}-${lineNo}`.toLowerCase();
    const key = raw
      .replace(/[^a-z0-9._-]+/g, "-")
      .replace(/-+/g, "-")
      .replace(/^-|-$/g, "");
    return key || `cluster-${Date.now()}-${Math.floor(Math.random() * 1000)}`;
  }

  function parseHostPort(text, lineNo) {
    const idx = text.lastIndexOf(":");
    if (idx <= 0 || idx === text.length - 1)
      throw new Error(t("batch.errors.hostPortFormatError", { lineNo }));
    const host = text.slice(0, idx).trim();
    const port = Number(text.slice(idx + 1).trim());
    if (!host || !Number.isInteger(port) || port <= 0 || port > 65535) {
      throw new Error(t("batch.errors.hostPortFormatError", { lineNo }));
    }
    return [host, port];
  }

  function parseBatchLine(raw, lineNo) {
    const parts = raw
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
    let name = "";
    let host = "";
    let port = 0;

    if (parts.length === 1) {
      if (!parts[0].includes(":"))
        throw new Error(
          t("batch.errors.singleColumnMustBeHostPort", { lineNo }),
        );
      [host, port] = parseHostPort(parts[0], lineNo);
      name = `task-${host}-${port}`;
    } else if (parts.length === 2) {
      if (parts[0].includes(":")) {
        [host, port] = parseHostPort(parts[0], lineNo);
        name = parts[1];
      } else {
        host = parts[0];
        port = Number(parts[1]);
        name = `task-${host}-${port}`;
      }
    } else if (parts.length === 3) {
      name = parts[0];
      host = parts[1];
      port = Number(parts[2]);
    } else {
      throw new Error(t("batch.errors.onlySupportFormats", { lineNo }));
    }

    if (
      !host ||
      !Number.isInteger(Number(port)) ||
      Number(port) <= 0 ||
      Number(port) > 65535
    ) {
      throw new Error(t("batch.errors.portInvalid", { lineNo }));
    }
    if (!name) name = `task-${host}-${port}`;
    const clusterKey = makeClusterKey(name, host, Number(port), lineNo);
    const clusterKeyErr = validateTaskPayload({
      name,
      cluster_key: clusterKey,
      source: {
        host,
        port: Number(port),
        user: "repl",
        flavor: "mysql",
        server_id: 1,
      },
      start: { mode: "LATEST" },
      storage: { retention_days: 7 },
    });
    if (clusterKeyErr)
      throw new Error(
        t("batch.errors.clusterKeyInvalid", { lineNo, error: clusterKeyErr }),
      );
    return { lineNo, name, host, port: Number(port), clusterKey };
  }

  function parseBatchLines(rawText) {
    const rows = [];
    const errors = [];
    const sourceSeen = new Set();
    const nameSeen = new Set();
    const clusterKeySeen = new Set();
    const lines = rawText.split("\n");
    for (let i = 0; i < lines.length; i += 1) {
      const lineNo = i + 1;
      const raw = lines[i].trim();
      if (!raw || raw.startsWith("#")) continue;
      try {
        const row = parseBatchLine(raw, lineNo);
        const sourceKey = `${row.host}:${row.port}`;
        const nameKey = row.name.toLowerCase();
        if (sourceSeen.has(sourceKey))
          throw new Error(
            t("batch.errors.sourceDuplicate", { lineNo, source: sourceKey }),
          );
        if (nameSeen.has(nameKey))
          throw new Error(
            t("batch.errors.nameDuplicate", { lineNo, name: row.name }),
          );
        if (clusterKeySeen.has(row.clusterKey))
          throw new Error(
            t("batch.errors.clusterKeyDuplicate", {
              lineNo,
              key: row.clusterKey,
            }),
          );
        sourceSeen.add(sourceKey);
        nameSeen.add(nameKey);
        clusterKeySeen.add(row.clusterKey);
        rows.push({ ...row, valid: true, error: "" });
      } catch (err) {
        const msg =
          err?.message || t("batch.errors.lineFormatError", { lineNo });
        rows.push({
          lineNo,
          name: "--",
          host: "--",
          port: "--",
          valid: false,
          error: msg,
        });
        errors.push(msg);
      }
    }
    if (!rows.length) errors.push(t("batch.noData"));
    return { rows, errors };
  }

  function previewBatchCreate() {
    const parsed = parseBatchLines(batchForm.lines || "");
    batchPreview.rows = parsed.rows;
    const validCount = parsed.rows.filter((x) => x.valid).length;
    const tooManyError =
      validCount > MAX_BATCH_ITEMS
        ? t("batch.tooManyItems", { max: MAX_BATCH_ITEMS })
        : "";
    batchPreview.errors = tooManyError
      ? [...parsed.errors, tooManyError]
      : parsed.errors;
    batchPreview.validCount = validCount;
    batchPreview.ready = true;
    batchPreview.canSubmit =
      batchPreview.validCount > 0 && batchPreview.errors.length === 0;
    if (tooManyError) {
      ElMessage.error(tooManyError);
      return;
    }
    if (!batchPreview.rows.length || batchPreview.errors.length > 0) {
      ElMessage.warning(
        t("msg.previewComplete", {
          valid: batchPreview.validCount,
          errors: batchPreview.errors.length,
        }),
      );
      return;
    }
    ElMessage.success(
      t("msg.previewPassed", { count: batchPreview.validCount }),
    );
  }

  async function submitBatchCreate() {
    try {
      const validRows = batchPreview.rows.filter((r) => r.valid);
      const errors = [];
      let success = 0;
      const submittedRows = [];
      const payloads = [];
      for (const row of validRows) {
        const source = {
          host: row.host,
          port: row.port,
          user: batchForm.user,
          flavor: batchForm.flavor,
          server_id: batchForm.serverIdStart + row.lineNo - 1,
          semi_sync: !!batchForm.semiSync,
        };
        if (batchForm.password) source.password = batchForm.password;
        const payload = {
          name: row.name,
          cluster_key: row.clusterKey,
          source,
          start: { mode: "LATEST" },
          storage: { retention_days: Number(batchForm.retentionDays) },
        };
        const validationErr = validateTaskPayload(payload);
        if (validationErr) {
          errors.push(
            `${row.name}(${row.host}:${row.port}) -> ${validationErr}`,
          );
          continue;
        }
        submittedRows.push(row);
        payloads.push(payload);
      }

      if (payloads.length) {
        const results = await createTasksBatch({ items: payloads });
        for (const result of results) {
          const row = submittedRows[Number(result?.index)];
          const label = row
            ? `${row.name}(${row.host}:${row.port})`
            : result?.cluster_key || "batch item";
          if (result?.task) {
            try {
              if (batchForm.autoStart) await startTask(result.task.id);
              success += 1;
            } catch (err) {
              errors.push(`${label} -> ${parseErr(err)}`);
            }
            continue;
          }
          const itemError = result?.error;
          const message =
            typeof itemError === "string"
              ? itemError
              : itemError?.error || JSON.stringify(itemError || "unknown error");
          errors.push(`${label} -> ${message}`);
        }
      }
      await refreshAll();
      if (!errors.length) {
        ElMessage.success(t("msg.batchCreateSuccess", { count: success }));
        batchVisible.value = false;
        return;
      }
      ElMessage.warning(
        t("msg.batchPartialSuccess", { success, failed: errors.length }),
      );
      await ElMessageBox.alert(
        errors.join("\n"),
        t("msg.batchCreateFailedDetail"),
        { confirmButtonText: t("btn.gotIt") },
      );
    } catch (err) {
      ElMessage.error(parseErr(err));
    }
  }

  return {
    batchVisible,
    batchForm,
    batchPreview,
    openBatchCreate,
    previewBatchCreate,
    submitBatchCreate,
    clearBatchPreview,
  };
}
