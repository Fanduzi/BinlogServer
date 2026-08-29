<!--
input: dashboard summary counters, active quick-filter state, i18n labels
output: first-screen KPI cards for task status and replication counters
pos: dashboard presentation component for operator-visible metrics
note: if this file changes, update this header and frontend/src/README.md
-->
<template>
  <section class="metric-grid">
    <article
      v-for="card in cards"
      :key="card.filter"
      class="metric-card"
      :class="card.variant ? `metric-card--${card.variant}` : ''"
      role="button"
      tabindex="0"
      :data-testid="card.testid"
      :data-active="activeQuickFilter === card.filter"
      @click="$emit('filter', card.filter)"
      @keydown.enter.prevent="$emit('filter', card.filter)"
      @keydown.space.prevent="$emit('filter', card.filter)"
    >
      <p><i :class="card.icon" /> {{ $t(card.labelKey) }}</p>
      <strong :data-testid="card.valuetestid">{{ summary[card.field] }}</strong>
    </article>
  </section>
</template>

<script setup>
defineProps({
  summary: { type: Object, required: true },
  activeQuickFilter: { type: String, required: true },
});
defineEmits(['filter']);

const cards = [
  { filter: 'abnormal', variant: 'danger',  testid: 'kpi-abnormal', valuetestid: 'kpi-abnormal-value', icon: 'fa-solid fa-triangle-exclamation', labelKey: 'metrics.abnormal', field: 'abnormal' },
  { filter: 'failed',   variant: 'danger',  testid: 'kpi-failed',   valuetestid: 'kpi-failed-value',   icon: 'fa-solid fa-bug',                   labelKey: 'metrics.failed',   field: 'failed'   },
  { filter: 'delayed',  variant: 'warning', testid: 'kpi-delayed',  valuetestid: 'kpi-delayed-value',  icon: 'fa-solid fa-hourglass-half',        labelKey: 'metrics.delayed',  field: 'delayed'  },
  { filter: 'starting', variant: 'warning', testid: 'kpi-starting', valuetestid: 'kpi-starting-value', icon: 'fa-solid fa-spinner',                labelKey: 'metrics.starting', field: 'starting' },
  { filter: 'running',  variant: '',        testid: 'kpi-running',  valuetestid: null,                 icon: 'fa-solid fa-play',                  labelKey: 'metrics.running',  field: 'running'  },
  { filter: 'all',      variant: '',        testid: 'kpi-all',      valuetestid: null,                 icon: 'fa-solid fa-layer-group',           labelKey: 'metrics.total',    field: 'total'    },
  { filter: 'normal',   variant: 'success', testid: 'kpi-normal',   valuetestid: null,                 icon: 'fa-solid fa-circle-check',          labelKey: 'metrics.normal',   field: 'normal'   },
];
</script>
