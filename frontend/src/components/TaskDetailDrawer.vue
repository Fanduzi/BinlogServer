<template>
  <el-drawer
    :model-value="visible"
    class="task-detail-drawer"
    data-testid="task-drawer"
    size="66%"
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
              <el-button data-testid="task-action-edit" @click="$emit('edit', task)">{{ $t('btn.edit') }}</el-button>
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
            <div class="detail-item"><span>{{ $t('detail.startMode') }}</span><strong>{{ task.start?.mode || "--" }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.semiSync') }}</span><strong>{{ task.source?.semi_sync ? $t('detail.on') : $t('detail.off') }}</strong></div>
            <div class="detail-item"><span>{{ $t('form.retentionDays') }}</span><strong>{{ task.storage?.retention_days || "--" }}</strong></div>
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

        <section class="detail-panel">
          <h3><i class="fa-solid fa-file-lines" /> {{ $t('detail.filesAndUpload') }}</h3>
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
          <el-table :data="files" size="small" border>
            <el-table-column prop="file_name" :label="$t('table.file')" min-width="180" />
            <el-table-column prop="size_bytes" :label="$t('table.size')" width="100" />
            <el-table-column prop="start_pos" :label="$t('table.startPos')" width="100" />
            <el-table-column prop="end_pos" :label="$t('table.endPos')" width="100" />
            <el-table-column prop="upload_state" :label="$t('table.uploadState')" width="130">
              <template #default="{ row }">
                <span :data-testid="`file-upload-state-${row.file_name}`">{{ row.upload_state }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="object_key" :label="$t('table.objectKey')" min-width="190" />
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
defineProps({
  visible: { type: Boolean, required: true },
  task: { type: Object, default: null },
  replication: { type: Object, default: null },
  lease: { type: Object, default: null },
  checkpoint: { type: Object, default: null },
  files: { type: Array, default: () => [] },
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
});
defineEmits(['update:visible', 'edit', 'start', 'stop', 'delete', 'retry-upload']);
</script>
