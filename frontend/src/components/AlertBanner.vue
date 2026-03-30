<template>
  <el-alert
    data-testid="auth-required-banner"
    class="auth-required-banner"
    type="warning"
    show-icon
    :closable="false"
    :title="title"
    :description="message"
  />
</template>

<script setup>
defineProps({
  title: { type: String, required: true },
  message: { type: String, required: true },
});
</script>
