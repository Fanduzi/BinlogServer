// Package api provides module-level functionality for api.
// input: GET /api/tasks/{id}/coverage?at=|gtid=, GET /api/coverage?source=host:port&at=|gtid=, POST /api/tasks/{id}/restore-plan with at, gtid, or latest and an optional full_backup_gtid
// output: the coverage JSON or the restore plan JSON, 200 when the point is outside the window, 400 for a bad query, 404 for an unknown task, 405 for the wrong method, 501 when the process is in cluster mode
// pos: HTTP for ADR 0009 phases 4 and 5; the decision stays in assurance.Cover
// note: if this file changes, update this header and module README.md.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"binlog_server/internal/tasks"
)

func (s *Server) handleCoverageAction(w http.ResponseWriter, r *http.Request, taskID, action string) {
	switch action {
	case "coverage":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleTaskCoverage(w, r, taskID)
	case "restore-plan":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleRestorePlan(w, r, taskID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleTaskCoverage(w http.ResponseWriter, r *http.Request, taskID string) {
	q, err := coverageQuery(r.URL.Query().Get("at"), r.URL.Query().Get("gtid"), false)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	cov, err := s.tasks.Coverage(taskID, q)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cov)
}

func (s *Server) handleSourceCoverage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := splitCoverageSource(r.URL.Query().Get("source"))
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	q, err := coverageQuery(r.URL.Query().Get("at"), r.URL.Query().Get("gtid"), false)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	got, err := s.tasks.CoverageBySource(host, port, q)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *Server) handleRestorePlan(w http.ResponseWriter, r *http.Request, taskID string) {
	var body struct {
		At             string `json:"at"`
		GTID           string `json:"gtid"`
		Latest         bool   `json:"latest"`
		FullBackupGTID string `json:"full_backup_gtid"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	q, err := coverageQuery(body.At, body.GTID, body.Latest)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	plan, err := s.tasks.RestorePlan(taskID, q, body.FullBackupGTID)
	if err != nil {
		writeCoverageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func coverageQuery(atRaw, gtidRaw string, latest bool) (tasks.CoverageQuery, error) {
	atRaw = strings.TrimSpace(atRaw)
	gtidRaw = strings.TrimSpace(gtidRaw)
	var q tasks.CoverageQuery
	if atRaw != "" {
		at, err := tasks.ParsePITRDatetime(atRaw)
		if err != nil {
			return tasks.CoverageQuery{}, tasks.ErrInvalidCoverageAt
		}
		q.At = &at
	}
	q.GTID = gtidRaw
	q.Latest = latest
	return q, nil
}

func splitCoverageSource(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	colon := strings.LastIndex(raw, ":")
	if colon <= 0 || colon == len(raw)-1 {
		return "", 0, tasks.ErrInvalidCoverageSource
	}
	host := strings.TrimSpace(raw[:colon])
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	port, err := strconv.Atoi(raw[colon+1:])
	if err != nil || host == "" || port < 1 || port > 65535 {
		return "", 0, tasks.ErrInvalidCoverageSource
	}
	return host, port, nil
}

func writeCoverageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tasks.ErrTaskNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, tasks.ErrCoverageUnsupported):
		http.Error(w, err.Error(), http.StatusNotImplemented)
	case errors.Is(err, tasks.ErrCoverageQuery),
		errors.Is(err, tasks.ErrCoverageExclusive),
		errors.Is(err, tasks.ErrRestoreTarget),
		errors.Is(err, tasks.ErrInvalidCoverageAt),
		errors.Is(err, tasks.ErrInvalidCoverageGTID),
		errors.Is(err, tasks.ErrCoverageGTIDFlavor),
		errors.Is(err, tasks.ErrInvalidCoverageSource),
		errors.Is(err, tasks.ErrInvalidStopGTID):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		msg := err.Error()
		if msg == "invalid gtid" || msg == "at and gtid cannot both be set" || msg == "at or gtid is required" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		writeServerError(w, err)
	}
}
