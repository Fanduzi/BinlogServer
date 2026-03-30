<template>
  <el-dialog
    :model-value="visible"
    :title="$t('batch.title')"
    :width="isMobile ? '95vw' : '860px'"
    @update:model-value="$emit('update:visible', $event)"
  >
    <el-alert type="info" show-icon :closable="false">
      <p>{{ $t('batch.formatHint') }}</p>
      <p><code>name,host,port</code> {{ $t('batch.formatOr') }} <code>host,port</code> {{ $t('batch.formatOr') }} <code>host:port</code></p>
    </el-alert>
    <el-row :gutter="12" class="batch-grid">
      <el-col :span="12">
        <el-form label-width="98px">
          <el-form-item :label="$t('form.replUser')"><el-input v-model="batchForm.user" /></el-form-item>
          <el-form-item :label="$t('form.replPassword')"><el-input v-model="batchForm.password" show-password /></el-form-item>
          <el-form-item :label="$t('form.flavor')"><el-input v-model="batchForm.flavor" /></el-form-item>
          <el-form-item :label="$t('form.serverIdStart')"><el-input-number v-model="batchForm.serverIdStart" :min="1" /></el-form-item>
          <el-form-item :label="$t('form.retentionDays')"><el-input-number v-model="batchForm.retentionDays" :min="1" /></el-form-item>
          <el-form-item :label="$t('form.semiSync')"><el-switch v-model="batchForm.semiSync" /></el-form-item>
          <el-form-item :label="$t('form.autoStart')"><el-switch v-model="batchForm.autoStart" /></el-form-item>
        </el-form>
      </el-col>
      <el-col :span="12">
        <el-input v-model="batchForm.lines" type="textarea" :rows="14" :placeholder="$t('placeholder.hostExample')" />
      </el-col>
      <el-col :span="24">
        <div class="batch-preview-toolbar">
          <div class="batch-preview-summary">
            <span>{{ $t('batch.previewStatus') }}</span>
            <el-tag v-if="batchPreview.ready && batchPreview.rows.length" type="info" size="small">{{ $t('batch.totalRows', { n: batchPreview.rows.length }) }}</el-tag>
            <el-tag v-if="batchPreview.ready && batchPreview.canSubmit" type="success" size="small">{{ $t('batch.allPassed') }}</el-tag>
            <el-tag v-if="batchPreview.ready && !batchPreview.canSubmit" type="danger" size="small">{{ $t('batch.hasFailed') }}</el-tag>
          </div>
          <el-button @click="$emit('preview')">{{ $t('btn.previewValidate') }}</el-button>
        </div>
        <el-table v-if="batchPreview.rows.length" :data="batchPreview.rows" size="small" border class="batch-preview-table">
          <el-table-column prop="lineNo" :label="$t('table.lineNo')" width="80" />
          <el-table-column prop="name" :label="$t('form.taskName')" min-width="180" />
          <el-table-column prop="host" :label="$t('batch.host')" min-width="160" />
          <el-table-column prop="port" :label="$t('batch.port')" width="100" />
          <el-table-column :label="$t('table.validation')" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.valid ? 'success' : 'danger'">{{ row.valid ? $t('batch.passed') : $t('batch.failed') }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="error" :label="$t('batch.reason')" min-width="220" />
        </el-table>
      </el-col>
    </el-row>
    <template #footer>
      <el-button @click="$emit('update:visible', false)">{{ $t('btn.cancel') }}</el-button>
      <el-button type="primary" :disabled="!batchPreview.canSubmit" @click="$emit('submit')">{{ $t('btn.startBatchCreate') }}</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
defineProps({
  visible: { type: Boolean, required: true },
  isMobile: { type: Boolean, default: false },
  batchForm: { type: Object, required: true },
  batchPreview: { type: Object, required: true },
});
defineEmits(['update:visible', 'preview', 'submit']);
</script>
