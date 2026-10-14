// Package api provides module-level functionality for api.
// input: the scheduler's finding sink snapshot and the stored open findings
// output: binlog_server_finding_open{task_id,kind,severity}, binlog_server_findings_reported_total{kind}, binlog_server_finding_sink_errors_total, binlog_server_finding_sink_pending, binlog_server_finding_store_active (ADR 0009)
// pos: metrics half of the finding sink; a finding is visible here even when its store write failed
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"sort"

	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
)

type findingMetricsSource interface {
	FindingStats() tasks.FindingStats
	OpenFindings(ctx context.Context, limit int) ([]tasks.Finding, error)
}

type findingCollector struct {
	src       findingMetricsSource
	ctx       context.Context
	open      *prometheus.Desc
	reported  *prometheus.Desc
	sinkErr   *prometheus.Desc
	pending   *prometheus.Desc
	storeLive *prometheus.Desc
}

func newFindingCollector(src findingMetricsSource, ctx context.Context) *findingCollector {
	return &findingCollector{
		src: src,
		ctx: ctx,
		open: prometheus.NewDesc("binlog_server_finding_open",
			"Open findings (ADR 0009). 1 per open (task_id, kind, subject); summed per task_id, kind, severity.",
			[]string{"task_id", "kind", "severity"}, nil),
		reported: prometheus.NewDesc("binlog_server_findings_reported_total",
			"Findings reported by this process since start, by kind.",
			[]string{"kind"}, nil),
		sinkErr: prometheus.NewDesc("binlog_server_finding_sink_errors_total",
			"Finding store or finding event writes that failed in this process. The finding stays open in memory and is retried.",
			nil, nil),
		pending: prometheus.NewDesc("binlog_server_finding_sink_pending",
			"Finding writes waiting for the store in this process.",
			nil, nil),
		storeLive: prometheus.NewDesc("binlog_server_finding_store_active",
			"1 when findings are persisted (task_findings table or the standalone file), 0 when they are kept in memory only.",
			nil, nil),
	}
}

func (c *findingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.open
	ch <- c.reported
	ch <- c.sinkErr
	ch <- c.pending
	ch <- c.storeLive
}

func (c *findingCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.src.FindingStats()
	// Store rows (every process) merged with this process's unwritten ones.
	// A store read error still exports this process's open findings, so the
	// gauge never goes silent.
	open, err := c.src.OpenFindings(c.ctx, 1000)
	if err != nil {
		open = stats.Open
	}
	type key struct{ task, kind, sev string }
	counts := map[key]int{}
	for _, f := range open {
		counts[key{f.TaskID, string(f.Kind), string(f.Severity)}]++
	}
	keys := make([]key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].task != keys[j].task {
			return keys[i].task < keys[j].task
		}
		return keys[i].kind+keys[i].sev < keys[j].kind+keys[j].sev
	})
	for _, k := range keys {
		ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(counts[k]), k.task, k.kind, k.sev)
	}
	if len(keys) == 0 {
		// Always one series so absent() alerts only on a dead process.
		ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, 0, "", "", "")
	}
	kinds := make([]string, 0, len(stats.Reported))
	for k := range stats.Reported {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		ch <- prometheus.MustNewConstMetric(c.reported, prometheus.CounterValue, float64(stats.Reported[tasks.FindingKind(k)]), k)
	}
	ch <- prometheus.MustNewConstMetric(c.sinkErr, prometheus.CounterValue, float64(stats.SinkErrors))
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(stats.PendingOps))
	active := 0.0
	if stats.StoreActive {
		active = 1
	}
	ch <- prometheus.MustNewConstMetric(c.storeLive, prometheus.GaugeValue, active)
}
