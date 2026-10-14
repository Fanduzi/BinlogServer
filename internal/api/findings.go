// Package api provides module-level functionality for api.
// input: the scheduler's finding sink (ADR 0009)
// output: GET /api/tasks/{id}/findings (open first; all=1 includes resolved) and GET /api/findings (open findings of every task, plus _cluster)
// pos: read side of the finding sink for the Consoles and operators
// note: if this file changes, update this header and module README.md.
package api

import (
	"context"
	"net/http"
	"strconv"

	"binlog_server/internal/tasks"
)

type findingReader interface {
	ListFindings(ctx context.Context, taskID string, includeResolved bool, limit int) ([]tasks.Finding, error)
	OpenFindings(ctx context.Context, limit int) ([]tasks.Finding, error)
}

func findingLimit(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 200, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > 1000 {
		return 0, false
	}
	return n, true
}

// handleTaskFindings godoc
// @Summary List a task's findings
// @Description Persisted failures for one task (ADR 0009). Open findings first. all=1 also returns resolved ones.
// @Tags tasks
// @Produce json
// @Param id path string true "task id"
// @Param all query bool false "include resolved findings"
// @Param limit query int false "1..1000, default 200"
// @Success 200 {array} tasks.Finding
// @Router /api/tasks/{id}/findings [get]
func (s *Server) handleTaskFindings(w http.ResponseWriter, r *http.Request, taskID string) {
	reader, ok := s.tasks.(findingReader)
	if !ok {
		writeJSON(w, http.StatusOK, []tasks.Finding{})
		return
	}
	limit, ok := findingLimit(r)
	if !ok {
		http.Error(w, "invalid limit: must be an integer in 1..1000", http.StatusBadRequest)
		return
	}
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	items, err := reader.ListFindings(r.Context(), taskID, all, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []tasks.Finding{}
	}
	writeJSON(w, http.StatusOK, items)
}

// handleOpenFindings godoc
// @Summary List open findings
// @Description Open findings of every task, and of the cluster (task_id _cluster). ADR 0009.
// @Tags tasks
// @Produce json
// @Param limit query int false "1..1000, default 200"
// @Success 200 {array} tasks.Finding
// @Router /api/findings [get]
func (s *Server) handleOpenFindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reader, ok := s.tasks.(findingReader)
	if !ok {
		writeJSON(w, http.StatusOK, []tasks.Finding{})
		return
	}
	limit, ok := findingLimit(r)
	if !ok {
		http.Error(w, "invalid limit: must be an integer in 1..1000", http.StatusBadRequest)
		return
	}
	items, err := reader.OpenFindings(r.Context(), limit)
	if err != nil && len(items) == 0 {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []tasks.Finding{}
	}
	writeJSON(w, http.StatusOK, items)
}
