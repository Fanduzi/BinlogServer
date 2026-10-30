// Package api provides module-level functionality for api.
// input: the scheduler's verifier snapshot (last finished pass per task)
// output: binlog_server_task_last_verified_timestamp, binlog_server_verify_bytes_total, binlog_server_verify_pass_incomplete, and the unverifiable, backfill and deferred gauges (ADR 0009 phase 3)
// pos: metrics half of the phase 3 verifier; the drill success gauge is not emitted while the drill is off
// note: if this file changes, update this header and module README.md.
package api

import (
	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
)

type verifyMetricsSource interface {
	VerifyMetrics() tasks.VerifyMetrics
}

type verifyCollector struct {
	src          verifyMetricsSource
	lastVerified *prometheus.Desc
	bytes        *prometheus.Desc
	incomplete   *prometheus.Desc
	unverifiable *prometheus.Desc
	backfill     *prometheus.Desc
	sizeUnknown  *prometheus.Desc
	deferred     *prometheus.Desc
	drillSuccess *prometheus.Desc
}

func newVerifyCollector(src verifyMetricsSource) *verifyCollector {
	return &verifyCollector{
		src: src,
		lastVerified: prometheus.NewDesc("binlog_server_task_last_verified_timestamp",
			"Unix time of the last verifier pass that was not cancelled, had no ERROR copy and deferred nothing.",
			[]string{"task_id"}, nil),
		bytes: prometheus.NewDesc("binlog_server_verify_bytes_total",
			"Bytes the verifier has read.",
			[]string{"task_id", "source"}, nil),
		incomplete: prometheus.NewDesc("binlog_server_verify_pass_incomplete",
			"1 when the last finished pass left this scope incomplete.",
			[]string{"task_id", "scope"}, nil),
		unverifiable: prometheus.NewDesc("binlog_server_verify_objects_unverifiable",
			"Objects with no digest to compare.",
			[]string{"task_id"}, nil),
		backfill: prometheus.NewDesc("binlog_server_verify_digest_backfill_pending",
			"Local files that still have no catalog digest.",
			[]string{"task_id"}, nil),
		sizeUnknown: prometheus.NewDesc("binlog_server_verify_objects_size_unverifiable",
			"Objects whose size cannot be compared.",
			[]string{"task_id"}, nil),
		deferred: prometheus.NewDesc("binlog_server_verify_deferred",
			"Copies scheduled this pass that were not paid for.",
			[]string{"task_id"}, nil),
		drillSuccess: prometheus.NewDesc("binlog_server_restore_drill_last_success_timestamp",
			"Unix time of the last restore drill that passed. Not emitted while no drill has passed.",
			[]string{"task_id"}, nil),
	}
}

func (c *verifyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.lastVerified
	ch <- c.bytes
	ch <- c.incomplete
	ch <- c.unverifiable
	ch <- c.backfill
	ch <- c.sizeUnknown
	ch <- c.deferred
	ch <- c.drillSuccess
}

func (c *verifyCollector) Collect(ch chan<- prometheus.Metric) {
	snap := c.src.VerifyMetrics()
	for id, m := range snap.Tasks {
		if m.HaveLastVerified {
			ch <- prometheus.MustNewConstMetric(c.lastVerified, prometheus.GaugeValue, float64(m.LastVerifiedUnix), id)
		}
		ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.CounterValue, float64(m.LocalBytes), id, "local")
		ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.CounterValue, float64(m.ObjectBytes), id, "object")
		ch <- prometheus.MustNewConstMetric(c.incomplete, prometheus.GaugeValue, boolGauge(m.IncompleteLocal), id, "local")
		ch <- prometheus.MustNewConstMetric(c.incomplete, prometheus.GaugeValue, boolGauge(m.IncompleteObject), id, "object")
		ch <- prometheus.MustNewConstMetric(c.incomplete, prometheus.GaugeValue, boolGauge(m.IncompleteChain), id, "chain")
		ch <- prometheus.MustNewConstMetric(c.unverifiable, prometheus.GaugeValue, float64(m.UnverifiableNoDigest), id)
		ch <- prometheus.MustNewConstMetric(c.backfill, prometheus.GaugeValue, float64(m.NoDigestLocal), id)
		ch <- prometheus.MustNewConstMetric(c.sizeUnknown, prometheus.GaugeValue, float64(m.UnverifiableSize), id)
		ch <- prometheus.MustNewConstMetric(c.deferred, prometheus.GaugeValue, float64(m.Deferred), id)
		if m.HaveDrillSuccess {
			ch <- prometheus.MustNewConstMetric(c.drillSuccess, prometheus.GaugeValue, float64(m.LastDrillSuccessUnix), id)
		}
	}
}

func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
