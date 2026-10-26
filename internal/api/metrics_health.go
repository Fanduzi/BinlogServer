// Package api provides module-level functionality for api.
// input: HealthSamples from the scheduler's evaluator
// output: binlog_server_task_healthy{task_id,reason}, binlog_server_task_progress_age_seconds{task_id}, binlog_server_task_upload_lag_seconds{task_id}, binlog_server_task_open_critical{task_id}; one task_id="" reason=ok series of 1 when this process has no evaluated tasks
// pos: F2 metrics half; the value is the evaluator's HealthOf verdict, not a second health check
// note: if this file changes, update this header and module README.md.
package api

import (
	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
)

type healthMetricsSource interface {
	HealthSamples() []tasks.HealthSample
}

type healthCollector struct {
	src          healthMetricsSource
	healthy      *prometheus.Desc
	progressAge  *prometheus.Desc
	uploadLag    *prometheus.Desc
	openCritical *prometheus.Desc
}

func newHealthCollector(src healthMetricsSource) *healthCollector {
	return &healthCollector{
		src: src,
		healthy: prometheus.NewDesc("binlog_server_task_healthy",
			"1 when the evaluator's verdict for this task is HEALTHY, else 0. reason is ok, critical, warning, unknown, unevaluated, or unread. Exported for every task this process evaluates.",
			[]string{"task_id", "reason"}, nil),
		progressAge: prometheus.NewDesc("binlog_server_task_progress_age_seconds",
			"Seconds since the last source byte or local progress advance on a desired-RUN task, from the pass that produced task_healthy. 0 when the task is not desired RUN or that pass had no clock.",
			[]string{"task_id"}, nil),
		uploadLag: prometheus.NewDesc("binlog_server_task_upload_lag_seconds",
			"Age in seconds of the oldest sealed segment that is not UPLOADED with checksum match. 0 when object storage is off or every sealed segment matches.",
			[]string{"task_id"}, nil),
		openCritical: prometheus.NewDesc("binlog_server_task_open_critical",
			"Open CRITICAL findings on the pass that produced task_healthy.",
			[]string{"task_id"}, nil),
	}
}

func (c *healthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.healthy
	ch <- c.progressAge
	ch <- c.uploadLag
	ch <- c.openCritical
}

func (c *healthCollector) Collect(ch chan<- prometheus.Metric) {
	var samples []tasks.HealthSample
	if c.src != nil {
		samples = c.src.HealthSamples()
	}
	if len(samples) == 0 {
		ch <- prometheus.MustNewConstMetric(c.healthy, prometheus.GaugeValue, 1, "", "ok")
		return
	}
	for _, s := range samples {
		reason := s.Reason
		if reason == "" {
			reason = "unevaluated"
		}
		ch <- prometheus.MustNewConstMetric(c.healthy, prometheus.GaugeValue, s.Value, s.TaskID, reason)
		ch <- prometheus.MustNewConstMetric(c.progressAge, prometheus.GaugeValue, s.ProgressAge, s.TaskID)
		ch <- prometheus.MustNewConstMetric(c.uploadLag, prometheus.GaugeValue, s.UploadLag, s.TaskID)
		ch <- prometheus.MustNewConstMetric(c.openCritical, prometheus.GaugeValue, float64(s.OpenCritical), s.TaskID)
	}
}
