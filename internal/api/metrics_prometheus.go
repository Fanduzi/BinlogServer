// Package api provides module-level functionality for api.
// input: scheduler/task service snapshots and progress used for metric exposition
// output: Prometheus collector and /metrics handler wiring with stable metric contracts, including at-tip replication lag of 0
// pos: observability edge for control-plane metrics exposure in API layer
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"binlog_server/internal/tasks"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const allFilesLimit = int(^uint(0) >> 1)

type apiMetricsCollector struct {
	tasks taskService

	taskStateCountDesc      *prometheus.Desc
	replicationLagSeconds   *prometheus.Desc
	checkpointAgeSeconds    *prometheus.Desc
	workerOnlineDesc        *prometheus.Desc
	uploadFailuresTotalDesc *prometheus.Desc
	uploadRetryTotalDesc    *prometheus.Desc
	uploadRetryLastTsGauge  *prometheus.Desc
}

func newAPIMetricsCollector(taskSvc taskService) *apiMetricsCollector {
	return &apiMetricsCollector{
		tasks: taskSvc,
		taskStateCountDesc: prometheus.NewDesc(
			"binlog_server_task_state_count",
			"Number of tasks by state.",
			[]string{"state"},
			nil,
		),
		replicationLagSeconds: prometheus.NewDesc(
			"binlog_server_replication_lag_seconds",
			"Replication lag seconds by task.",
			[]string{"task_id"},
			nil,
		),
		checkpointAgeSeconds: prometheus.NewDesc(
			"binlog_server_checkpoint_age_seconds",
			"Checkpoint age seconds by task.",
			[]string{"task_id"},
			nil,
		),
		workerOnlineDesc: prometheus.NewDesc(
			"binlog_server_worker_online",
			"Worker online status (1=online,0=offline).",
			[]string{"worker_id"},
			nil,
		),
		uploadFailuresTotalDesc: prometheus.NewDesc(
			"binlog_server_upload_failures_total",
			"Total number of upload failed file records.",
			nil,
			nil,
		),
		uploadRetryTotalDesc: prometheus.NewDesc(
			"binlog_server_upload_retry_total",
			"Total retry-upload API result count.",
			[]string{"result"},
			nil,
		),
		uploadRetryLastTsGauge: prometheus.NewDesc(
			"binlog_server_upload_retry_last_ts",
			"Last retry-upload API execution time in unix seconds.",
			nil,
			nil,
		),
	}
}

func (c *apiMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.taskStateCountDesc
	ch <- c.replicationLagSeconds
	ch <- c.checkpointAgeSeconds
	ch <- c.workerOnlineDesc
	ch <- c.uploadFailuresTotalDesc
	ch <- c.uploadRetryTotalDesc
	ch <- c.uploadRetryLastTsGauge
}

func (c *apiMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	items := c.tasks.ListTasks()
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	stateCount := make(map[string]int)
	for _, task := range items {
		stateCount[string(task.State)]++
	}
	states := make([]string, 0, len(stateCount))
	for state := range stateCount {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		ch <- prometheus.MustNewConstMetric(c.taskStateCountDesc, prometheus.GaugeValue, float64(stateCount[state]), state)
	}

	replicationLagEmitted := false
	for _, task := range items {
		progress, ok, err := c.tasks.GetReplicationProgress(task.ID)
		if err != nil || !ok || progress.LastEventAt.IsZero() {
			continue
		}
		lag := now.Sub(progress.LastEventAt).Seconds()
		if progress.AtTip || lag < 0 {
			lag = 0
		}
		ch <- prometheus.MustNewConstMetric(c.replicationLagSeconds, prometheus.GaugeValue, lag, task.ID)
		replicationLagEmitted = true
	}
	if !replicationLagEmitted {
		ch <- prometheus.MustNewConstMetric(c.replicationLagSeconds, prometheus.GaugeValue, 0, "")
	}

	checkpointAgeEmitted := false
	for _, task := range items {
		checkpoint, ok, err := c.tasks.GetCheckpoint(context.Background(), task.ID)
		if err != nil || !ok || checkpoint.UpdatedAt.IsZero() {
			continue
		}
		age := now.Sub(checkpoint.UpdatedAt).Seconds()
		if age < 0 {
			age = 0
		}
		ch <- prometheus.MustNewConstMetric(c.checkpointAgeSeconds, prometheus.GaugeValue, age, task.ID)
		checkpointAgeEmitted = true
	}
	if !checkpointAgeEmitted {
		ch <- prometheus.MustNewConstMetric(c.checkpointAgeSeconds, prometheus.GaugeValue, 0, "")
	}

	heartbeats, err := c.tasks.ListWorkerHeartbeats(200)
	if err == nil {
		workerOnlineEmitted := false
		sort.Slice(heartbeats, func(i, j int) bool {
			return heartbeats[i].WorkerID < heartbeats[j].WorkerID
		})
		for _, hb := range heartbeats {
			online := strings.EqualFold(hb.Status, "ONLINE") && !hb.LastSeenAt.IsZero() && now.Sub(hb.LastSeenAt) <= workerOnlineThreshold
			value := 0.0
			if online {
				value = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.workerOnlineDesc, prometheus.GaugeValue, value, hb.WorkerID)
			workerOnlineEmitted = true
		}
		if !workerOnlineEmitted {
			ch <- prometheus.MustNewConstMetric(c.workerOnlineDesc, prometheus.GaugeValue, 0, "")
		}
	}

	ch <- prometheus.MustNewConstMetric(c.uploadFailuresTotalDesc, prometheus.GaugeValue, float64(countUploadFailures(c.tasks, items)))

	retryMetrics := c.tasks.GetUploadRetryMetrics()
	ch <- prometheus.MustNewConstMetric(c.uploadRetryTotalDesc, prometheus.CounterValue, float64(retryMetrics.Success), "success")
	ch <- prometheus.MustNewConstMetric(c.uploadRetryTotalDesc, prometheus.CounterValue, float64(retryMetrics.Failed), "failed")
	ch <- prometheus.MustNewConstMetric(c.uploadRetryTotalDesc, prometheus.CounterValue, float64(retryMetrics.Skipped), "skipped")
	ch <- prometheus.MustNewConstMetric(c.uploadRetryLastTsGauge, prometheus.GaugeValue, float64(retryMetrics.LastTs))
}

func countUploadFailures(taskSvc taskService, items []tasks.Task) int64 {
	if counter, ok := taskSvc.(interface{ CountUploadFailures() (int64, error) }); ok {
		total, err := counter.CountUploadFailures()
		if err == nil {
			return total
		}
	}

	var uploadFailuresTotal int64
	for _, task := range items {
		files, err := taskSvc.ListFiles(task.ID, allFilesLimit)
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.EqualFold(file.UploadState, "UPLOAD_FAILED") {
				uploadFailuresTotal++
			}
		}
	}
	return uploadFailuresTotal
}

func newMetricsHandler(taskSvc taskService) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(newAPIMetricsCollector(taskSvc))
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
