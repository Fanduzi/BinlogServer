<template>
  <aside class="nav-pane" :class="{ 'nav-pane--collapsed': modelValue }">
    <div class="nav-head">
      <div class="nav-brand" :title="modelValue ? $t('nav.brand') : ''">
        <span class="nav-brand-icon"><i class="fa-solid fa-wave-square" /></span>
        <span class="nav-brand-text">{{ $t('nav.brand') }}</span>
      </div>
    </div>
    <button
      class="nav-item"
      :class="{ 'nav-item--active': activeView === 'overview' }"
      data-testid="view-nav-overview"
      @click="$emit('switchView', 'overview')"
    >
      <span><i class="fa-solid fa-border-all" /><span class="nav-label">{{ $t('nav.overview') }}</span></span>
      <span class="nav-badge">{{ totalCount }}</span>
    </button>
    <button
      class="nav-item"
      :class="{ 'nav-item--active': activeView === 'tasks' }"
      data-testid="view-nav-tasks"
      @click="$emit('switchView', 'tasks')"
    >
      <span><i class="fa-solid fa-table-list" /><span class="nav-label">{{ $t('nav.tasks') }}</span></span>
      <span class="nav-badge">{{ filteredTasksCount }}</span>
    </button>
    <button
      class="nav-item"
      :class="{ 'nav-item--active': activeView === 'sources' }"
      data-testid="view-nav-sources"
      @click="$emit('switchView', 'sources')"
    >
      <span><i class="fa-solid fa-database" /><span class="nav-label">{{ $t('nav.sources') }}</span></span>
      <span class="nav-badge">{{ sourcesCount }}</span>
    </button>
    <button
      class="nav-item"
      :class="{ 'nav-item--active': activeView === 'workers' }"
      data-testid="view-nav-workers"
      @click="$emit('switchView', 'workers')"
    >
      <span><i class="fa-solid fa-network-wired" /><span class="nav-label">{{ $t('nav.workers') }}</span></span>
      <span class="nav-badge">{{ workerCount }}</span>
    </button>
    <button
      class="nav-item"
      :class="{ 'nav-item--active': activeView === 'alerts' }"
      data-testid="view-nav-alerts"
      @click="$emit('switchView', 'alerts')"
    >
      <span><i class="fa-solid fa-bell" /><span class="nav-label">{{ $t('nav.alerts') }}</span></span>
      <span class="nav-badge">{{ alertCount }}</span>
    </button>
    <div class="nav-foot">
      <button
        class="nav-collapse-btn nav-collapse-btn--dock"
        :title="modelValue ? $t('btn.expandMenu') : $t('btn.collapseMenu')"
        :aria-label="modelValue ? $t('btn.expandMenu') : $t('btn.collapseMenu')"
        data-testid="view-nav-collapse"
        @click="$emit('update:modelValue', !modelValue)"
      >
        <i :class="modelValue ? 'fa-solid fa-angles-right' : 'fa-solid fa-angles-left'" aria-hidden="true" />
      </button>
    </div>
  </aside>
</template>

<script setup>
defineProps({
  modelValue: { type: Boolean, required: true },
  activeView: { type: String, required: true },
  totalCount: { type: Number, required: true },
  filteredTasksCount: { type: Number, required: true },
  sourcesCount: { type: Number, required: true },
  workerCount: { type: Number, required: true },
  alertCount: { type: Number, required: true },
});
defineEmits(['update:modelValue', 'switchView']);
</script>
