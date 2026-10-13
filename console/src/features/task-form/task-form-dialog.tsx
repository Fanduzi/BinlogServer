// Create / edit / adopt dialog (TaskCreateDialog.vue + useTaskForm.js).
// TaskFormProvider owns one dialog for the whole app; the header opens it for
// create, the task drawer for edit or adopt (a leftover directory always adopts).
import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { useObservation } from "@/context/observation";
import { adoptTask, createTask, updateTask, type AnyRecord } from "@/lib/api";
import { useI18n } from "@/lib/i18n";
import { isLeftoverDiskTask, parseErr } from "../task-detail/detail-format";
import {
  ADOPT_START_DEFAULT,
  RETENTION_DAYS_MAX,
  adoptForm,
  buildAdoptPayload,
  buildPayload,
  defaultForm,
  editForm,
  validateAdoptPayload,
  validateTaskPayload,
  type FormMode,
  type TaskForm,
} from "./task-form";

type Done = () => void | Promise<void>;

interface TaskFormApi {
  openCreate: () => void;
  // Edit, or adopt when the task is a leftover directory. onDone runs after a successful save.
  openEdit: (task: AnyRecord, onDone?: Done) => void;
}

const TaskFormContext = createContext<TaskFormApi | null>(null);

export function useTaskForm() {
  const ctx = useContext(TaskFormContext);
  if (!ctx) throw new Error("useTaskForm outside TaskFormProvider");
  return ctx;
}

export function TaskFormProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n();
  const { refresh } = useObservation();
  const [open, setOpen] = useState(false);
  const [mode, setMode] = useState<FormMode>("create");
  const [form, setForm] = useState<TaskForm>(defaultForm);
  const [saving, setSaving] = useState(false);
  const doneRef = useRef<Done | undefined>(undefined);

  const openCreate = useCallback(() => {
    doneRef.current = undefined;
    setMode("create");
    setForm(defaultForm());
    setOpen(true);
  }, []);

  const openEdit = useCallback((task: AnyRecord, onDone?: Done) => {
    doneRef.current = onDone;
    if (isLeftoverDiskTask(task)) {
      setMode("adopt");
      setForm(adoptForm(task));
    } else {
      setMode("edit");
      setForm(editForm(task));
    }
    setOpen(true);
  }, []);

  async function submit() {
    if (saving) return;
    const adopting = mode === "adopt";
    const payload = adopting ? buildAdoptPayload(form) : buildPayload(form);
    const validationErr = adopting
      ? validateAdoptPayload(t, payload)
      : validateTaskPayload(t, payload);
    if (validationErr) {
      toast.error(validationErr);
      return;
    }
    setSaving(true);
    try {
      if (mode === "create") {
        await createTask(payload);
        toast.success(t("msg.taskCreated"));
      } else if (adopting) {
        await adoptTask(form.id, payload);
        toast.success(t("msg.taskAdopted"));
      } else {
        await updateTask(form.id, payload);
        toast.success(t("msg.taskUpdated"));
      }
      setForm((f) => ({ ...f, source: { ...f.source, password: "" } }));
      setOpen(false);
      await refresh();
      await doneRef.current?.();
    } catch (err) {
      toast.error(parseErr(err));
    } finally {
      setSaving(false);
    }
  }

  const api = useMemo(() => ({ openCreate, openEdit }), [openCreate, openEdit]);
  const title =
    mode === "create"
      ? t("btn.createTask")
      : `${mode === "adopt" ? t("btn.adopt") : t("btn.edit")} #${form.id}`;

  return (
    <TaskFormContext.Provider value={api}>
      {children}
      <Dialog open={open} onOpenChange={(v) => !saving && setOpen(v)}>
        <DialogContent
          data-testid="task-form"
          className="max-h-[90vh] overflow-y-auto sm:max-w-[860px]"
        >
          <DialogHeader>
            <DialogTitle data-testid="task-form-title">{title}</DialogTitle>
            <DialogDescription className="sr-only">{title}</DialogDescription>
          </DialogHeader>
          <form
            className="grid grid-cols-1 gap-3 sm:grid-cols-2"
            onSubmit={(e) => {
              e.preventDefault();
              void submit();
            }}
          >
            <FormFields mode={mode} form={form} setForm={setForm} />
            <button type="submit" hidden />
          </form>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => setOpen(false)}
            >
              {t("btn.cancel")}
            </Button>
            <Button
              data-testid="task-form-submit"
              disabled={saving}
              onClick={() => void submit()}
            >
              {saving && <Loader2 className="animate-spin" />}
              {mode === "adopt" ? t("btn.adopt") : t("btn.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </TaskFormContext.Provider>
  );
}

function Field({
  label,
  hint,
  className,
  children,
}: {
  label: string;
  hint?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={`flex min-w-0 flex-col gap-1.5 ${className ?? ""}`}>
      <Label>{label}</Label>
      {children}
      {hint && (
        <p className="text-muted-foreground text-xs leading-snug">{hint}</p>
      )}
    </div>
  );
}

const num = (v: string) => (v.trim() === "" ? 0 : Number(v));

function FormFields({
  mode,
  form,
  setForm,
}: {
  mode: FormMode;
  form: TaskForm;
  setForm: React.Dispatch<React.SetStateAction<TaskForm>>;
}) {
  const { t } = useI18n();
  const top = (patch: Partial<TaskForm>) =>
    setForm((f) => ({ ...f, ...patch }));
  const src = (patch: Partial<TaskForm["source"]>) =>
    setForm((f) => ({ ...f, source: { ...f.source, ...patch } }));
  const start = (patch: Partial<TaskForm["start"]>) =>
    setForm((f) => ({ ...f, start: { ...f.start, ...patch } }));
  const store = (patch: Partial<TaskForm["storage"]>) =>
    setForm((f) => ({ ...f, storage: { ...f.storage, ...patch } }));

  return (
    <>
      <Field label={t("form.taskName")}>
        <Input
          data-testid="task-form-name"
          value={form.name}
          onChange={(e) => top({ name: e.target.value })}
        />
      </Field>
      <Field label={t("form.clusterKey")}>
        <Input
          data-testid="task-form-cluster-key"
          value={form.cluster_key ?? ""}
          onChange={(e) => top({ cluster_key: e.target.value })}
        />
      </Field>
      <Field label={t("form.host")}>
        <Input
          data-testid="task-form-host"
          value={form.source.host ?? ""}
          onChange={(e) => src({ host: e.target.value })}
        />
      </Field>
      <Field label={t("form.port")}>
        <Input
          data-testid="task-form-port"
          type="number"
          min={1}
          max={65535}
          value={form.source.port}
          onChange={(e) => src({ port: num(e.target.value) })}
        />
      </Field>
      <Field label={t("form.user")}>
        <Input
          data-testid="task-form-user"
          value={form.source.user ?? ""}
          onChange={(e) => src({ user: e.target.value })}
        />
      </Field>
      <Field label={t("form.password")}>
        <Input
          data-testid="task-form-password"
          type="password"
          autoComplete="new-password"
          placeholder={
            mode === "adopt"
              ? t("form.passwordRequiredHint")
              : t("form.passwordHint")
          }
          value={form.source.password ?? ""}
          onChange={(e) => src({ password: e.target.value })}
        />
      </Field>
      <Field label={t("form.serverId")}>
        <Input
          data-testid="task-form-server-id"
          type="number"
          min={1}
          value={form.source.server_id}
          onChange={(e) => src({ server_id: num(e.target.value) })}
        />
      </Field>
      <Field label={t("form.flavor")} hint={t("form.flavorHint")}>
        <Select
          value={form.source.flavor || "mysql"}
          onValueChange={(v) => src({ flavor: v })}
        >
          <SelectTrigger data-testid="task-form-flavor" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="mysql">{t("form.flavorMysql")}</SelectItem>
            <SelectItem value="mariadb">{t("form.flavorMariaDB")}</SelectItem>
          </SelectContent>
        </Select>
      </Field>
      <Field label={t("form.semiSync")}>
        <Switch
          data-testid="task-form-semi-sync"
          checked={!!form.source.semi_sync}
          onCheckedChange={(v) => src({ semi_sync: v })}
        />
      </Field>
      <div className="grid grid-cols-1 gap-3 sm:col-span-2 sm:grid-cols-3">
        <Field label={t("form.retentionDays")}>
          <Input
            data-testid="task-form-retention"
            type="number"
            min={1}
            max={RETENTION_DAYS_MAX}
            value={form.storage.retention_days}
            onChange={(e) => store({ retention_days: num(e.target.value) })}
          />
        </Field>
        <Field label={t("form.localRetentionDays")}>
          <Input
            data-testid="task-form-local-retention"
            type="number"
            min={0}
            max={RETENTION_DAYS_MAX}
            value={form.storage.local_retention_days ?? 0}
            onChange={(e) =>
              store({ local_retention_days: num(e.target.value) })
            }
          />
        </Field>
        <Field
          label={t("form.bucketRetentionDays")}
          hint={t("form.retentionSplitHint")}
        >
          <Input
            data-testid="task-form-bucket-retention"
            type="number"
            min={0}
            max={RETENTION_DAYS_MAX}
            value={form.storage.bucket_retention_days ?? 0}
            onChange={(e) =>
              store({ bucket_retention_days: num(e.target.value) })
            }
          />
        </Field>
        <Field
          label={t("form.startMode")}
          hint={mode === "adopt" ? t("form.adoptHint") : undefined}
        >
          <Select
            value={form.start.mode}
            onValueChange={(v) => start({ mode: v })}
          >
            <SelectTrigger
              data-testid="task-form-start-mode"
              className="w-full"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {mode === "adopt" && (
                <SelectItem value={ADOPT_START_DEFAULT}>
                  {t("form.startModeDefault")}
                </SelectItem>
              )}
              <SelectItem value="LATEST">LATEST</SelectItem>
              <SelectItem value="FILE_POS">FILE_POS</SelectItem>
              <SelectItem value="GTID">GTID</SelectItem>
            </SelectContent>
          </Select>
        </Field>
        <Field label={t("form.file")}>
          <Input
            data-testid="task-form-file"
            value={form.start.file ?? ""}
            onChange={(e) => start({ file: e.target.value })}
          />
        </Field>
        <Field label={t("form.position")}>
          <Input
            data-testid="task-form-pos"
            type="number"
            min={0}
            value={form.start.pos ?? 0}
            onChange={(e) => start({ pos: num(e.target.value) })}
          />
        </Field>
      </div>
      <Field label={t("form.gtidSet")} className="sm:col-span-2">
        <Input
          data-testid="task-form-gtid"
          value={form.start.gtid_set ?? ""}
          onChange={(e) => start({ gtid_set: e.target.value })}
        />
      </Field>
    </>
  );
}
