<template>
  <el-dialog
    :model-value="visible"
    :title="mode === 'create' ? $t('btn.createTask') : mode === 'adopt' ? `${$t('btn.adopt')} #${form.id}` : `${$t('btn.edit')} #${form.id}`"
    :width="isMobile ? '95vw' : '860px'"
    @update:model-value="$emit('update:visible', $event)"
  >
    <el-form :model="form" label-width="92px">
      <el-row :gutter="12">
        <el-col :span="12"><el-form-item :label="$t('form.taskName')"><el-input v-model="form.name" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.clusterKey')"><el-input v-model="form.cluster_key" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.host')"><el-input v-model="form.source.host" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.port')"><el-input-number v-model="form.source.port" :min="1" :max="65535" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.user')"><el-input v-model="form.source.user" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.password')"><el-input v-model="form.source.password" show-password :placeholder="mode === 'adopt' ? $t('form.passwordRequiredHint') : $t('form.passwordHint')" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.serverId')"><el-input-number v-model="form.source.server_id" :min="1" /></el-form-item></el-col>
        <el-col :span="12">
          <el-form-item :label="$t('form.flavor')">
            <el-select v-model="form.source.flavor" style="width: 100%">
              <el-option value="mysql" :label="$t('form.flavorMysql')" />
              <el-option value="mariadb" :label="$t('form.flavorMariaDB')" />
            </el-select>
            <p class="flavor-hint">{{ $t('form.flavorHint') }}</p>
          </el-form-item>
        </el-col>
        <el-col :span="12"><el-form-item :label="$t('form.semiSync')"><el-switch v-model="form.source.semi_sync" /></el-form-item></el-col>
        <el-col :span="12"><el-form-item :label="$t('form.retentionDays')"><el-input-number v-model="form.storage.retention_days" :min="1" /></el-form-item></el-col>
        <el-col :span="8">
          <el-form-item :label="$t('form.startMode')">
            <el-select v-model="form.start.mode" style="width: 100%">
              <el-option v-if="mode === 'adopt'" :value="ADOPT_START_DEFAULT" :label="$t('form.startModeDefault')" />
              <el-option value="LATEST" label="LATEST" />
              <el-option value="FILE_POS" label="FILE_POS" />
              <el-option value="GTID" label="GTID" />
            </el-select>
            <p v-if="mode === 'adopt'" class="flavor-hint">{{ $t('form.adoptHint') }}</p>
          </el-form-item>
        </el-col>
        <el-col :span="8"><el-form-item :label="$t('form.file')"><el-input v-model="form.start.file" /></el-form-item></el-col>
        <el-col :span="8"><el-form-item :label="$t('form.position')"><el-input-number v-model="form.start.pos" :min="0" /></el-form-item></el-col>
        <el-col :span="24"><el-form-item :label="$t('form.gtidSet')"><el-input v-model="form.start.gtid_set" /></el-form-item></el-col>
      </el-row>
    </el-form>
    <template #footer>
      <el-button @click="$emit('update:visible', false)">{{ $t('btn.cancel') }}</el-button>
      <el-button type="primary" data-testid="task-form-submit" @click="$emit('submit')">{{ mode === 'adopt' ? $t('btn.adopt') : $t('btn.save') }}</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
import { ADOPT_START_DEFAULT } from "../composables/useTaskForm.js";

defineProps({
  visible: { type: Boolean, required: true },
  isMobile: { type: Boolean, default: false },
  mode: { type: String, default: 'create' },
  form: { type: Object, required: true },
});
defineEmits(['update:visible', 'submit']);
</script>

<style scoped>
.flavor-hint {
  margin: 6px 0 0;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  line-height: 1.4;
}
</style>
