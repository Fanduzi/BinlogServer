<template>
  <el-dialog
    :model-value="visible"
    class="settings-dialog"
    data-testid="settings-dialog"
    :title="$t('settings.title')"
    :width="isMobile ? '95vw' : '480px'"
    @update:model-value="$emit('update:visible', $event)"
  >
    <el-form class="settings-form" label-width="100px">
      <el-form-item :label="$t('settings.language')">
        <el-select :model-value="currentLocale" @change="$emit('locale-change', $event)">
          <el-option :label="$t('settings.langZhCN')" value="zh-CN" />
          <el-option :label="$t('settings.langEn')" value="en" />
        </el-select>
      </el-form-item>
      <el-form-item :label="$t('settings.apiToken')">
        <el-input
          :model-value="token"
          data-testid="settings-token-input"
          type="password"
          show-password
          :placeholder="$t('auth.tokenPlaceholder')"
          @update:model-value="$emit('update:token', $event)"
        />
        <div class="form-hint">{{ $t('auth.tokenHint') }}</div>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="$emit('update:visible', false)">{{ $t('btn.cancel') }}</el-button>
      <el-button type="primary" @click="$emit('save')">{{ $t('btn.save') }}</el-button>
    </template>
  </el-dialog>
</template>

<script setup>
defineProps({
  visible: { type: Boolean, required: true },
  isMobile: { type: Boolean, default: false },
  currentLocale: { type: String, required: true },
  token: { type: String, default: '' },
});
defineEmits(['update:visible', 'update:token', 'locale-change', 'save']);
</script>
