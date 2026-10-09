<!--
input: task, replication, checkpoint, the recoverable window, storage_alert, source_chain on the task, source_identity on each file, locale labels, and loadPitr for the datetime window and stop_gtid
output: task detail drawer with configured start identity, the resume file:pos / GTID, a localized storage_alert (damaged segment, missing GTIDs, segments that still restore, restart gtid_set) when the checkpoint or a segment disagrees with the stored transactions, a warning on replay and point-in-time results that left the damaged segment out, the retained chain's earliest and latest UTC times, a warning when that chain has a break, the source-server chain and a continued or stopped switchover notice, a point-in-time replay command, a stop_gtid replay command, and a warning when a source Binlog Dump connection is still pending KILL
pos: operator view of the position the next Start continues from and the datetime or GTID restore drill
note: if this file changes, update this header and frontend/src/components/README.md
-->
<template>
  <el-drawer
    :model-value="visible"
    class="task-detail-drawer"
    data-testid="task-drawer"
    :size="isMobile ? '100%' : '66%'"
    :title="task ? `${$t('detail.center')} #${task.id}` : $t('detail.center')"
    @update:model-value="$emit('update:visible', $event)"
  >
    <template v-if="task">
      <div class="detail-stack">
        <section class="detail-panel detail-panel--hero">
          <div class="detail-hero">
            <div>
              <div class="detail-hero-kicker">{{ $t('detail.center') }}</div>
              <h3><i class="fa-solid fa-circle-info" /> {{ task.name }}</h3>
              <div class="detail-hero-meta">
                <el-tag data-testid="task-drawer-status" :type="stateTagType(task.state)">{{ stateLabel(task.state) }}</el-tag>
                <el-tag v-if="replication" data-testid="task-drawer-replication" :type="replicationTagType(replication.status)">{{ replicationStatusLabel(replication.status) }}</el-tag>
                <span data-testid="task-drawer-source">{{ $t('detail.source') }} {{ sourceLabel(task) }}</span>
                <span>{{ $t('detail.clusterKey') }} {{ task.cluster_key || "--" }}</span>
              </div>
            </div>
            <div class="detail-action-row" data-testid="task-drawer-actions">
              <el-button v-if="isLeftover(task)" data-testid="task-action-adopt" type="primary" @click="$emit('adopt', task)">{{ $t('btn.adopt') }}</el-button>
              <el-button v-else data-testid="task-action-edit" @click="$emit('edit', task)">{{ $t('btn.edit') }}</el-button>
              <el-button data-testid="task-action-start" type="success" @click="$emit('start', task)">{{ $t('btn.start') }}</el-button>
              <el-button data-testid="task-action-stop" type="warning" @click="$emit('stop', task)">{{ $t('btn.stop') }}</el-button>
              <el-button data-testid="task-action-delete" type="danger" plain @click="$emit('delete', task)">{{ $t('btn.delete') }}</el-button>
            </div>
          </div>
          <div class="detail-grid detail-grid--summary">
            <div class="detail-item"><span>{{ $t('detail.currentState') }}</span><strong>{{ stateLabel(task.state) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.replicationStatus') }}</span><strong>{{ replication ? replicationStatusLabel(replication.status) : "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.delay') }}</span><strong>{{ replication ? `${formatDelay(replication.delay_seconds, replication.has_progress)} ${$t('detail.seconds')}` : "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.leaseStatus') }}</span><strong>{{ leaseRiskLabel(task, lease) }}</strong></div>
          </div>
          <el-alert
            v-if="task.storage_alert"
            data-testid="task-storage-alert"
            type="error"
            :closable="false"
            show-icon
            :title="$t('detail.storageAlert')"
          >
            <div class="storage-alert-body">
              <p v-for="(line, i) in storageAlertLines" :key="i" data-testid="task-storage-alert-line">{{ line }}</p>
            </div>
          </el-alert>
          <el-alert
            v-if="task.pending_dump_cleanup && task.pending_dump_cleanup.connection_id"
            data-testid="task-pending-dump-cleanup"
            class="detail-pending-dump"
            type="warning"
            :closable="false"
            show-icon
            :title="pendingDumpTitle(task)"
          />
          <el-alert
            v-if="showContinuedBanner"
            data-testid="task-source-continued"
            class="detail-pending-dump"
            type="warning"
            :closable="false"
            show-icon
            :title="$t('detail.sourceContinuedTitle')"
          >
            {{ $t('detail.sourceContinuedBody') }}
          </el-alert>
          <el-alert
            v-if="showStoppedBanner"
            data-testid="task-source-stopped"
            class="detail-pending-dump"
            type="error"
            :closable="false"
            show-icon
            :title="stoppedReasonText"
          >
            <p v-if="stoppedMove" data-testid="task-source-stopped-move">{{ stoppedMove }}</p>
            <p data-testid="task-source-next">{{ $t('detail.sourceStoppedNext') }}</p>
          </el-alert>
        </section>

        <section v-if="replication" class="detail-panel">
          <h3><i class="fa-solid fa-wave-square" /> {{ $t('detail.replicationAndPosition') }}</h3>
          <div class="detail-grid">
            <div class="detail-item">
              <span>{{ $t('detail.replicationStatus') }}</span>
              <strong><el-tag :type="replicationTagType(replication.status)">{{ replicationStatusLabel(replication.status) }}</el-tag></strong>
            </div>
            <div class="detail-item"><span>{{ $t('detail.delay') }}</span><strong>{{ formatDelay(replication.delay_seconds, replication.has_progress) }} {{ $t('detail.seconds') }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.checkpoint') }}</span><strong data-testid="task-drawer-checkpoint">{{ formatCheckpoint(checkpoint) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.errorReason') }}</span><strong>{{ formatReplicationReason(replication) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.alertThreshold') }}</span><strong>{{ replication.threshold_seconds || "--" }} {{ $t('detail.seconds') }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.lastEventTime') }}</span><strong>{{ formatTs(replication.last_event_at) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.lastPosition') }}</span><strong>{{ replication.last_event_file || "-" }}:{{ replication.last_event_pos || 0 }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.statusCode') }}</span><strong>{{ replication.reason || "--" }}</strong></div>
          </div>
        </section>

        <section class="detail-panel">
          <h3><i class="fa-solid fa-circle-info" /> {{ $t('detail.basicInfo') }}</h3>
          <div class="detail-grid">
            <div class="detail-item"><span>{{ $t('table.name') }}</span><strong>{{ task.name }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.clusterKey') }}</span><strong>{{ task.cluster_key || "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.source') }}</span><strong>{{ sourceLabel(task) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.startMode') }}</span><strong data-testid="task-drawer-start">{{ formatStart(task.start) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.checkpoint') }}</span><strong data-testid="task-drawer-resume">{{ formatCheckpoint(checkpoint) }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.semiSync') }}</span><strong>{{ task.source?.semi_sync ? $t('detail.on') : $t('detail.off') }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.retentionDays') }}</span><strong data-testid="task-drawer-retention">{{ task.storage?.retention_days || "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.localRetentionDays') }}</span><strong data-testid="task-drawer-local-retention">{{ effectiveRetention(task.storage, 'local') }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.bucketRetentionDays') }}</span><strong data-testid="task-drawer-bucket-retention">{{ effectiveRetention(task.storage, 'bucket') }}</strong></div>
          </div>
        </section>

        <section v-if="lease" class="detail-panel">
          <h3><i class="fa-solid fa-key" /> {{ $t('detail.leaseAndWorker') }}</h3>
          <div class="detail-grid">
            <div class="detail-item"><span>{{ $t('detail.ownerWorker') }}</span><strong data-testid="task-drawer-worker">{{ lease.owner_worker_id || "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.epoch') }}</span><strong>{{ lease.epoch || "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.leaseStatus') }}</span><strong>{{ leaseRiskLabel(task, lease) }}</strong></div>
            <div class="detail-item"><span>{{ $t('detail.updatedAt') }}</span><strong>{{ formatTs(lease.updated_at) }}</strong></div>
          </div>
        </section>

        <section v-if="showSourceChain" class="detail-panel" data-testid="task-source-chain">
          <h3><i class="fa-solid fa-link" /> {{ $t('detail.sourceChain') }}</h3>
          <p class="replay-set-hint">{{ $t('detail.sourceChainHint') }}</p>
          <ul class="source-chain-list">
            <li v-for="(server, index) in sourceServers" :key="`${server.identity}-${index}`" data-testid="task-source-server">
              {{ $t('detail.serverOrdinal', { n: index + 1 }) }}
              <code>{{ server.identity }}</code>
              <el-tag v-if="server.current" size="small" type="success" data-testid="task-source-current">{{ $t('detail.serverCurrent') }}</el-tag>
            </li>
          </ul>
          <ul v-if="sourceSwitches.length" class="source-chain-list">
            <li v-for="(sw, index) in sourceSwitches" :key="`${sw.old}-${sw.new}-${index}`" data-testid="task-source-switch">
              <span>{{ formatTs(sw.time) }}</span>
              <strong>{{ sw.old }} → {{ sw.new }}</strong>
              <span v-if="switchWhere(sw)">{{ switchWhere(sw) }}</span>
              <el-tag size="small" :type="sw.continued ? 'success' : 'danger'">
                {{ sw.continued ? $t('detail.switchContinued') : $t('detail.switchStopped') }}
              </el-tag>
            </li>
          </ul>
        </section>

        <section class="detail-panel">
          <h3><i class="fa-solid fa-file-lines" /> {{ $t('detail.filesAndUpload') }}</h3>
          <div class="recovery-window" data-testid="task-recovery-window">
            <div class="replay-set-head">
              <div class="replay-set-label">
                <strong>{{ $t('detail.recoveryWindow') }}</strong>
                <span class="replay-set-hint">{{ $t('detail.recoveryHint') }}</span>
              </div>
              <el-tag
                v-if="recovery"
                size="small"
                :type="recovery.continuous ? 'success' : 'warning'"
                data-testid="task-recovery-continuous"
              >
                {{ recovery.continuous ? $t('detail.recoveryContinuous') : $t('detail.recoveryBroken') }}
              </el-tag>
            </div>
            <p class="recovery-range" data-testid="task-recovery-range">{{ recoveryRange }}</p>
            <p v-if="recoveryGtid" class="replay-set-hint" data-testid="task-recovery-gtid">
              {{ $t('detail.recoveryGtid') }}: {{ recoveryGtid }}
            </p>
            <el-alert
              v-if="recoveryBreaks.length"
              type="warning"
              :closable="false"
              show-icon
              data-testid="task-recovery-breaks"
              :title="$t('detail.recoveryBreaks')"
            >
              <ul class="recovery-breaks">
                <li
                  v-for="(item, index) in recoveryBreaks"
                  :key="index"
                  data-testid="task-recovery-break"
                >
                  {{ formatBreak(item) }}
                </li>
              </ul>
            </el-alert>
          </div>
          <div class="replay-set" data-testid="task-replay">
            <div class="replay-set-head">
              <div class="replay-set-label">
                <strong>{{ $t('detail.replay') }}</strong>
                <span class="replay-set-hint" data-testid="task-replay-hint">{{ replayHint }}</span>
              </div>
              <div class="replay-set-actions">
                <el-button
                  data-testid="task-replay-copy"
                  size="small"
                  :disabled="!replayCommand"
                  @click="copyReplay"
                >
                  {{ $t('btn.copyReplay') }}
                </el-button>
                <el-button
                  data-testid="task-replay-download"
                  size="small"
                  @click="$emit('download-replay', task)"
                >
                  {{ $t('btn.downloadReplay') }}
                </el-button>
              </div>
            </div>
            <el-alert
              v-if="replayWarning"
              data-testid="task-replay-damaged"
              type="warning"
              :closable="false"
              show-icon
              :title="replayWarning"
            />
            <pre v-if="replayCommand" class="replay-set-command" data-testid="task-replay-command">{{ replayCommand }}</pre>
            <p v-else class="replay-set-empty" data-testid="task-replay-command">{{ $t('detail.replayEmpty') }}</p>
            <div class="pitr-set" data-testid="task-pitr">
              <div class="replay-set-label">
                <strong>{{ $t('detail.pitr') }}</strong>
                <span class="replay-set-hint">{{ $t('detail.pitrHint') }}</span>
              </div>
              <div class="pitr-fields">
                <el-input
                  v-model="pitrStop"
                  data-testid="task-pitr-stop"
                  size="small"
                  :placeholder="$t('detail.pitrStop')"
                />
                <el-input
                  v-model="pitrGtid"
                  class="pitr-gtid"
                  data-testid="task-pitr-gtid"
                  size="small"
                  :placeholder="$t('detail.pitrGtid')"
                />
                <el-input
                  v-model="pitrStart"
                  data-testid="task-pitr-start"
                  size="small"
                  :placeholder="$t('detail.pitrStart')"
                />
                <el-input
                  v-model="pitrExecuted"
                  class="pitr-gtid"
                  data-testid="task-pitr-executed"
                  size="small"
                  :placeholder="$t('detail.pitrExecuted')"
                />
                <el-button
                  data-testid="task-pitr-build"
                  size="small"
                  :disabled="!canBuildPitr"
                  @click="buildPitr"
                >
                  {{ $t('btn.buildPitr') }}
                </el-button>
                <el-button
                  data-testid="task-pitr-copy"
                  size="small"
                  :disabled="!pitrCommand"
                  @click="copyPitr"
                >
                  {{ $t('btn.copyPitr') }}
                </el-button>
                <el-button
                  data-testid="task-pitr-download"
                  size="small"
                  :disabled="!canBuildPitr"
                  @click="$emit('download-pitr', { task, stop: pitrStop.trim(), start: pitrStart.trim(), gtid: pitrGtid.trim(), executed: pitrExecuted.trim() })"
                >
                  {{ $t('btn.downloadPitr') }}
                </el-button>
              </div>
              <p v-if="pitrWarning" class="replay-set-empty" data-testid="task-pitr-damaged">{{ pitrWarning }}</p>
              <p v-if="pitrError" class="replay-set-empty" data-testid="task-pitr-error">{{ pitrError }}</p>
              <pre v-else-if="pitrCommand" class="replay-set-command" data-testid="task-pitr-command">{{ pitrCommand }}</pre>
              <p v-else-if="pitrNote" class="replay-set-empty" data-testid="task-pitr-command">{{ pitrNote }}</p>
              <p v-else-if="pitrResult" class="replay-set-empty" data-testid="task-pitr-command">{{ $t('detail.pitrEmpty') }}</p>
            </div>
          </div>
          <div class="detail-panel-toolbar">
            <el-button
              v-if="files.some((file) => file.upload_state === 'UPLOAD_FAILED')"
              data-testid="retry-upload-action"
              size="small"
              type="warning"
              @click="$emit('retry-upload', task)"
            >
              {{ $t('btn.retryUpload') }}
            </el-button>
          </div>
          <p v-if="showBucketOnly" class="replay-set-empty" data-testid="task-bucket-only-hint">{{ $t('detail.bucketOnly') }}</p>
          <el-table :data="files" size="small" border>
            <el-table-column :label="$t('table.file')" min-width="220">
              <template #default="{ row }">
                <span :data-testid="`file-disk-name-${diskBase(row)}`">{{ diskBase(row) }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="$t('table.server')" min-width="240">
              <template #default="{ row }">
                <span :data-testid="`file-source-${diskBase(row)}`">{{ fileServerLabel(row) }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="$t('table.filePath')" min-width="280">
              <template #default="{ row }">
                <span data-testid="file-disk-path">{{ row.file_path }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="size_bytes" :label="$t('table.size')" width="100" />
            <el-table-column prop="start_pos" :label="$t('table.startPos')" width="100" />
            <el-table-column prop="end_pos" :label="$t('table.endPos')" width="100" />
            <el-table-column :label="$t('table.location')" width="120">
              <template #default="{ row }">
                <span :data-testid="`file-location-${diskBase(row)}`">{{ locationLabel(row.location) }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="upload_state" :label="$t('table.uploadState')" width="130">
              <template #default="{ row }">
                <span :data-testid="`file-upload-state-${diskBase(row)}`">{{ row.upload_state }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="checksum" :label="$t('table.checksum')" width="110">
              <template #default="{ row }">
                <span :data-testid="`file-checksum-${diskBase(row)}`">{{ row.checksum || "--" }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="object_key" :label="$t('table.objectKey')" min-width="190" />
            <el-table-column :label="$t('table.actions')" width="128">
              <template #default="{ row }">
                <el-button
                  v-if="diskBase(row)"
                  size="small"
                  :data-testid="`file-download-${diskBase(row)}`"
                  @click="$emit('download-file', { task, name: diskBase(row) })"
                >
                  {{ $t('btn.download') }}
                </el-button>
              </template>
            </el-table-column>
          </el-table>
        </section>

        <section class="detail-panel" data-testid="task-drawer-runs">
          <h3><i class="fa-solid fa-clock-rotate-left" /> {{ $t('detail.runHistory', { limit: runHistoryLimit }) }}</h3>
          <el-table :data="runsLimited" size="small" border>
            <el-table-column prop="run_id" :label="$t('table.runId')" min-width="180" />
            <el-table-column prop="worker_id" :label="$t('table.worker')" min-width="120" />
            <el-table-column prop="epoch" :label="$t('table.epoch')" width="100" />
            <el-table-column :label="$t('table.startTime')" min-width="170">
              <template #default="{ row }">{{ formatTs(row.started_at) }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.endTime')" min-width="170">
              <template #default="{ row }">{{ formatTs(row.ended_at) }}</template>
            </el-table-column>
            <el-table-column :label="$t('table.endReason')" min-width="140">
              <template #default="{ row }">{{ row.end_reason || "--" }}</template>
            </el-table-column>
          </el-table>
          <el-empty v-if="runsLimited.length === 0" :description="$t('empty.noRunHistory')" :image-size="56" />
        </section>

        <section class="detail-panel" data-testid="task-drawer-events">
          <h3><i class="fa-solid fa-clock-rotate-left" /> {{ $t('detail.eventTimeline') }}</h3>
          <el-timeline>
            <el-timeline-item
              v-for="ev in events"
              :key="`${ev.sequence}-${ev.type}`"
              :timestamp="ev.time"
            >
              <strong>{{ ev.type }}</strong>
              <p>{{ ev.message }}</p>
            </el-timeline-item>
          </el-timeline>
        </section>
      </div>
    </template>
  </el-drawer>
</template>

<script setup>
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { ElMessage } from "element-plus";

const props = defineProps({
  visible: { type: Boolean, required: true },
  task: { type: Object, default: null },
  replication: { type: Object, default: null },
  lease: { type: Object, default: null },
  checkpoint: { type: Object, default: null },
  files: { type: Array, default: () => [] },
  replay: { type: Object, default: null },
  recovery: { type: Object, default: null },
  runsLimited: { type: Array, default: () => [] },
  events: { type: Array, default: () => [] },
  runHistoryLimit: { type: Number, default: 10 },
  stateTagType: { type: Function, required: true },
  stateLabel: { type: Function, required: true },
  replicationTagType: { type: Function, required: true },
  replicationStatusLabel: { type: Function, required: true },
  sourceLabel: { type: Function, required: true },
  leaseRiskLabel: { type: Function, required: true },
  formatDelay: { type: Function, required: true },
  formatCheckpoint: { type: Function, required: true },
  formatReplicationReason: { type: Function, required: true },
  formatTs: { type: Function, required: true },
  isLeftover: { type: Function, required: true },
  loadPitr: { type: Function, required: true },
  isMobile: { type: Boolean, default: false },
});
defineEmits(['update:visible', 'edit', 'adopt', 'start', 'stop', 'delete', 'retry-upload', 'download-file', 'download-replay', 'download-pitr']);

const { t } = useI18n();

const pitrStop = ref("");
const pitrGtid = ref("");
const pitrStart = ref("");
const pitrExecuted = ref("");
const pitrResult = ref(null);
const pitrError = ref("");

watch(() => props.task?.id, () => {
  pitrStop.value = "";
  pitrGtid.value = "";
  pitrStart.value = "";
  pitrExecuted.value = "";
  pitrResult.value = null;
  pitrError.value = "";
});

const canBuildPitr = computed(() => Boolean(pitrStop.value.trim() || pitrGtid.value.trim()));

const pitrCommand = computed(() => String(pitrResult.value?.command || ""));
const pitrNote = computed(() => String(pitrResult.value?.note || "").trim());

// storageAlertLines renders storage_alert in the Console language from its
// fields. An older server without segment falls back to its English message.
const storageAlertLines = computed(() => {
  const alert = props.task?.storage_alert;
  if (!alert) return [];
  const segment = String(alert.segment || "").trim();
  if (!segment) return [String(alert.message || "").trim()].filter(Boolean);
  const lines = [t("detail.storageAlertSegment", { segment })];
  const missing = String(alert.missing_gtids || "").trim();
  if (missing) lines.push(t("detail.storageAlertMissing", { gtids: missing }));
  const valid = Array.isArray(alert.valid_segments) ? alert.valid_segments : [];
  if (valid.length === 0) {
    lines.push(t("detail.storageAlertNoValid", { segment }));
  } else if (valid.length === 1) {
    lines.push(t("detail.storageAlertValidOne", { segment, first: valid[0] }));
  } else {
    lines.push(t("detail.storageAlertValid", { segment, count: valid.length, first: valid[0], last: valid[valid.length - 1] }));
  }
  lines.push(t("detail.storageAlertNext"));
  const restart = String(alert.restart_gtid_set || "").trim();
  if (restart) lines.push(t("detail.storageAlertRestart", { gtids: restart }));
  return lines;
});

// damagedReplayText is the localized replay warning when the server left a
// damaged segment out, or the server's text when the segment is unknown.
function damagedReplayText(warning) {
  const text = String(warning || "").trim();
  if (!text) return "";
  const segment = String(props.task?.storage_alert?.segment || "").trim();
  return segment ? t("detail.replayDamaged", { segment }) : text;
}

const replayWarning = computed(() => damagedReplayText(props.replay?.warning));
const pitrWarning = computed(() => damagedReplayText(pitrResult.value?.warning));

function formatRecoveryInstant(value) {
  const text = String(value || "").trim();
  if (!text) return "";
  return text.replace("T", " ").replace(/\.\d+/, "").replace(/Z$/, " UTC");
}

const recoveryRange = computed(() => {
  const earliest = formatRecoveryInstant(props.recovery?.earliest);
  const latest = formatRecoveryInstant(props.recovery?.latest);
  if (!earliest && !latest) return t("detail.recoveryEmpty");
  return `${earliest || "--"} → ${latest || "--"}`;
});

const recoveryGtid = computed(() => String(props.recovery?.gtid_set || "").trim());

const recoveryBreaks = computed(() => {
  const items = props.recovery?.breaks;
  return Array.isArray(items) ? items : [];
});

function formatBreak(item) {
  const files = Array.isArray(item?.files) ? item.files.filter(Boolean).join(", ") : "";
  const reason = String(item?.reason || "").trim();
  if (files && reason) return `${files}: ${reason}`;
  return reason || files;
}

const replayHint = computed(() => {
  const hint = String(props.replay?.client_hint || "").trim();
  return hint || t("detail.replayFlavorUnset");
});

const showBucketOnly = computed(() => {
  const rows = Array.isArray(props.files) ? props.files : [];
  if (rows.some((row) => row && row.location === "bucket")) return true;
  const sets = [props.replay, pitrResult.value];
  return sets.some((set) => Array.isArray(set?.locations) && set.locations.includes("bucket"));
});

const replayCommand = computed(() => formatReplayCommand(props.replay));

async function buildPitr() {
  const stop = pitrStop.value.trim();
  const gtid = pitrGtid.value.trim();
  if ((!stop && !gtid) || !props.task) return;
  pitrError.value = "";
  pitrResult.value = null;
  try {
    pitrResult.value = await props.loadPitr(props.task, stop, pitrStart.value.trim(), gtid, pitrExecuted.value.trim());
  } catch (err) {
    pitrError.value = pitrErrorText(err);
  }
}

function pitrErrorText(err) {
  const data = err?.response?.data;
  if (typeof data === "string" && data.trim()) return data.trim();
  if (data?.error) return String(data.error);
  return err?.message || t("msg.unknownError");
}

async function copyPitr() {
  const text = pitrCommand.value;
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success(t("msg.pitrCopied"));
  } catch {
    ElMessage.error(t("msg.pitrCopyFailed"));
  }
}

async function copyReplay() {
  const text = replayCommand.value;
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success(t("msg.replayCopied"));
  } catch {
    ElMessage.error(t("msg.replayCopyFailed"));
  }
}

function formatReplayCommand(replay) {
  const paths = Array.isArray(replay?.paths) ? replay.paths.filter(Boolean) : [];
  if (paths.length === 0) return "";
  const tokens = paths.map(shellToken);
  const client = String(replay?.client || "").trim();
  if (!client) return tokens.join("\n");
  return `${client} \\\n${tokens.map((token) => `  ${token}`).join(" \\\n")}`;
}

function shellToken(path) {
  const text = String(path);
  if (/[^A-Za-z0-9_./:@+-]/.test(text)) {
    return `'${text.replace(/'/g, `'\\''`)}'`;
  }
  return text;
}

function pendingDumpTitle(task) {
  const id = task?.pending_dump_cleanup?.connection_id;
  const base = t("detail.pendingDumpCleanup", { id });
  if (task?.pending_dump_cleanup?.process_local) {
    return `${base} ${t("detail.pendingDumpCleanupProcessLocal")}`;
  }
  return base;
}

function formatStart(start) {
  if (!start) return "--";
  const mode = String(start.mode || "").trim();
  if (!mode) return "--";
  if (mode === "FILE_POS") {
    return `FILE_POS ${start.file || "-"}:${start.pos ?? 0}`;
  }
  if (mode === "GTID") {
    const gtid = String(start.gtid_set || start.gtid || "").trim() || "--";
    return `GTID ${gtid}`;
  }
  return mode;
}

function effectiveRetention(storage, which) {
  const base = Number(storage?.retention_days || 0);
  const specific = Number(which === "local" ? storage?.local_retention_days : storage?.bucket_retention_days) || 0;
  const days = specific > 0 ? specific : base;
  return days > 0 ? String(days) : "--";
}

function locationLabel(location) {
  if (location === "bucket") return t("detail.locationBucket");
  if (location === "both") return t("detail.locationBoth");
  if (location === "local") return t("detail.locationLocal");
  return "--";
}

const sourceServers = computed(() => {
  const items = props.task?.source_chain?.servers;
  return Array.isArray(items) ? items : [];
});

const sourceSwitches = computed(() => {
  const items = props.task?.source_chain?.switches;
  return Array.isArray(items) ? items : [];
});

const showSourceChain = computed(() => sourceServers.value.length > 0 || sourceSwitches.value.length > 0);

const showStoppedBanner = computed(() => {
  if (props.task?.source_chain?.outcome === "stopped") return true;
  return String(props.task?.last_error || "").startsWith("SOURCE_SWITCHOVER");
});

const showContinuedBanner = computed(() => {
  return props.task?.source_chain?.outcome === "continued"
    && props.task?.state === "RUNNING"
    && !showStoppedBanner.value;
});

const latestStop = computed(() => {
  const stopped = sourceSwitches.value.filter((item) => item && item.continued === false);
  return stopped.length ? stopped[stopped.length - 1] : null;
});

const stoppedReasonText = computed(() => {
  const reason = String(latestStop.value?.reason || "").trim();
  if (!reason) return t("detail.switchReason.unknown");
  return t(`detail.switchReason.${reason}`);
});

const stoppedMove = computed(() => {
  const sw = latestStop.value;
  if (!sw?.old || !sw?.new) return "";
  return t("detail.switchFromTo", { old: sw.old, new: sw.new });
});

function switchWhere(sw) {
  const file = String(sw?.file || "").trim();
  const pos = Number(sw?.pos || 0);
  const gtid = String(sw?.gtid_set || "").trim();
  const parts = [];
  if (file && pos) parts.push(`${file}:${pos}`);
  else if (file) parts.push(file);
  if (gtid) parts.push(gtid);
  return parts.join(" · ");
}

function fileServerLabel(row) {
  const id = String(row?.source_identity || "").trim();
  if (!id) return "--";
  const index = sourceServers.value.findIndex((server) => server && server.identity === id);
  const ordinal = index >= 0 ? t("detail.serverOrdinal", { n: index + 1 }) : "";
  const current = index >= 0 && sourceServers.value[index].current ? t("detail.serverCurrent") : "";
  return [ordinal, id, current].filter(Boolean).join(" · ");
}

function diskBase(row) {
  const path = row && row.file_path ? String(row.file_path) : "";
  if (!path) {
    return (row && row.file_name) || "";
  }
  const parts = path.split(/[/\\]/);
  return parts[parts.length - 1] || ((row && row.file_name) || "");
}
</script>
