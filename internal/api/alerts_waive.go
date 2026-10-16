// Package api provides module-level functionality for api.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"binlog_server/internal/health"
)

// waiveRequest is POST /api/alerts/waive (design 0009 §22.3).
type waiveRequest struct {
	TaskID   string `json:"task_id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`
}

// handleAlertsWaive godoc
// @Summary Waive a GTID_HOLE finding
// @Description Binds exact canonical GTID evidence to a waiver (ADR 0009 §7.1 / §22.3). Only GTID_HOLE.
// @Tags alerts
// @Accept json
// @Produce json
// @Param body body waiveRequest true "waiver"
// @Success 200 {object} map[string]string
// @Failure 400 {string} string
// @Router /api/alerts/waive [post]
func (s *Server) handleAlertsWaive(w http.ResponseWriter, r *http.Request) {
	waver, ok := s.tasks.(interface {
		WaiveFinding(ctx context.Context, taskID, kind, subject, evidence, reason, actor string) error
		UnwaiveFinding(ctx context.Context, taskID, kind, subject string) error
	})
	if !ok {
		http.Error(w, "waivers are not available", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req waiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.TaskID = strings.TrimSpace(req.TaskID)
		req.Kind = strings.TrimSpace(req.Kind)
		req.Subject = strings.TrimSpace(req.Subject)
		req.Evidence = strings.TrimSpace(req.Evidence)
		req.Reason = strings.TrimSpace(req.Reason)
		if req.TaskID == "" || req.Kind == "" || req.Subject == "" || req.Evidence == "" || req.Reason == "" {
			http.Error(w, "task_id, kind, subject, evidence and reason are required", http.StatusBadRequest)
			return
		}
		if req.Kind != string(health.GTIDHole) {
			http.Error(w, "only GTID_HOLE is waivable", http.StatusBadRequest)
			return
		}
		actor := strings.TrimSpace(req.Actor)
		if actor == "" {
			actor = "operator"
		}
		if err := waver.WaiveFinding(r.Context(), req.TaskID, req.Kind, req.Subject, req.Evidence, req.Reason, actor); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "waived"})
	case http.MethodDelete:
		var req waiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.TaskID = strings.TrimSpace(req.TaskID)
		req.Kind = strings.TrimSpace(req.Kind)
		req.Subject = strings.TrimSpace(req.Subject)
		if req.TaskID == "" || req.Kind == "" || req.Subject == "" {
			http.Error(w, "task_id, kind and subject are required", http.StatusBadRequest)
			return
		}
		if err := waver.UnwaiveFinding(r.Context(), req.TaskID, req.Kind, req.Subject); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unwaived"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
