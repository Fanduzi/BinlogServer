<template>
  <header class="hero">
    <div class="hero-copy">
      <p class="kicker"><i class="fa-solid fa-wave-square" /> BINLOG SERVER</p>
      <h1>{{ $t('app.title') }}</h1>
      <p class="hero-desc">{{ $t('app.description') }}</p>
    </div>
    <div class="hero-actions">
      <el-button type="primary" @click="$emit('create')">
        <i class="fa-solid fa-plus" /> {{ $t('btn.createTask') }}
      </el-button>
      <el-button @click="$emit('batch-create')">
        <i class="fa-solid fa-layer-group" /> {{ $t('btn.batchCreate') }}
      </el-button>
      <el-button :loading="loading" @click="$emit('refresh')">
        <i class="fa-solid fa-rotate" /> {{ $t('btn.refresh') }}
      </el-button>
      <el-button @click="$emit('settings')">
        <i class="fa-solid fa-gear" /> {{ $t('btn.settings') }}
      </el-button>
    </div>
  </header>
</template>

<script setup>
defineProps({
  loading: { type: Boolean, default: false },
});
defineEmits(['create', 'batch-create', 'refresh', 'settings']);
</script>
