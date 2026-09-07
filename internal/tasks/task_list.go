// Package tasks provides module-level functionality for tasks.
// input: in-memory task snapshots, TaskListFilter host/port/state/limit/offset, and binlog file snapshots
// output: numeric-id-ordered filtered pages, COUNT totals, STARTING-unowned subsets, and UPLOAD_FAILED file subsets
// pos: shared list/filter/page helpers for TaskStore fakes and standalone Scheduler paging
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"sort"
	"strconv"
	"strings"
)

// TaskListFilter is the list/dashboard page contract pushed to SQL when a store is configured.
type TaskListFilter struct {
	Host   string
	Port   *uint16
	State  *State
	Limit  int
	Offset int
}

// FilterTasks applies cheap host/port/state predicates. Limit/Offset are ignored.
func FilterTasks(items []Task, filter TaskListFilter) []Task {
	out := make([]Task, 0, len(items))
	for _, task := range items {
		if filter.Host != "" && task.Source.Host != filter.Host {
			continue
		}
		if filter.Port != nil && task.Source.Port != *filter.Port {
			continue
		}
		if filter.State != nil && task.State != *filter.State {
			continue
		}
		out = append(out, task)
	}
	return out
}

// SortTasksByID orders tasks as ORDER BY CAST(id AS UNSIGNED), id.
func SortTasksByID(items []Task) {
	sort.SliceStable(items, func(i, j int) bool {
		return LessTaskID(items[i].ID, items[j].ID)
	})
}

// LessTaskID reports whether task id a should sort before b using numeric-then-string order.
func LessTaskID(a, b string) bool {
	ai, aOK := parseNumericTaskID(a)
	bi, bOK := parseNumericTaskID(b)
	switch {
	case aOK && bOK:
		if ai != bi {
			return ai < bi
		}
		return a < b
	case aOK:
		return true
	case bOK:
		return false
	default:
		return a < b
	}
}

func parseNumericTaskID(id string) (uint64, bool) {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// TaskPageBounds returns the [start, end) slice range for a page.
// limit <= 0 means the rest of the list (unbounded).
func TaskPageBounds(total, offset, limit int) (int, int) {
	if offset >= total {
		return total, total
	}
	if limit <= 0 {
		return offset, total
	}
	end := total
	if limit <= total-offset {
		end = offset + limit
	}
	return offset, end
}

// PaginateTasks slices a pre-sorted task list.
func PaginateTasks(items []Task, offset, limit int) []Task {
	start, end := TaskPageBounds(len(items), offset, limit)
	return items[start:end]
}

// PageTasks filters, sorts by numeric id, and pages. total is the filtered count, not the page length.
func PageTasks(items []Task, filter TaskListFilter) ([]Task, int) {
	filtered := FilterTasks(items, filter)
	SortTasksByID(filtered)
	return PaginateTasks(filtered, filter.Offset, filter.Limit), len(filtered)
}

// FailedUploadFiles returns UPLOAD_FAILED files, capped by limit when limit > 0.
func FailedUploadFiles(items []BinlogFile, limit int) []BinlogFile {
	out := make([]BinlogFile, 0)
	for _, item := range items {
		if !strings.EqualFold(item.UploadState, "UPLOAD_FAILED") {
			continue
		}
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// StartingUnownedTasks returns STARTING tasks whose owner_worker_id is empty.
func StartingUnownedTasks(items []Task) []Task {
	out := make([]Task, 0)
	for _, item := range items {
		if item.State != StateStarting {
			continue
		}
		if item.OwnerWorkerID != "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

// LookupTask returns a task by id or ErrTaskNotFound.
func LookupTask(items []Task, id string) (Task, error) {
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return Task{}, ErrTaskNotFound
}
