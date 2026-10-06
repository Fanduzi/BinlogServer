// Package tasks provides module-level functionality for tasks.
// input: the last pending Binlog Dump cleanup and the result of one KILL attempt
// output: the next pending marker, a held connection id another worker can fence, a process-local warning when the metadata column is absent, and whether that transition emits one pending or cleared event
// pos: state machine for a Stop whose KILL could not reach the source
// note: if this file changes, update this header and module README.md.
package tasks

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	dumpCleanupPending = "pending"
	dumpCleanupCleared = "cleared"
)

// ClusterDumpFenceSchemaMessage is the operator text when cluster mode has no
// shared pending_dump_cleanup column. Single-process schema 3 does not use it.
const ClusterDumpFenceSchemaMessage = "cluster mode needs migration 000004 (pending_dump_cleanup) before another worker can finish a dump it did not close; run ./migrate up"

// DumpCleanup is one Binlog Dump connection.
// Warning is filled in for the API. It is not stored.
// ProcessLocal is true when this process is the only copy (schema 3). It is not stored.
// Held is true while the owning worker still has this dump open. It is stored so
// another process can fence it. The API omits a held marker.
// Epoch is the task epoch that opened the connection. It is stored.
type DumpCleanup struct {
	ConnectionID uint32 `json:"connection_id"`
	Host         string `json:"host,omitempty"`
	Port         uint16 `json:"port,omitempty"`
	Warning      string `json:"warning,omitempty"`
	ProcessLocal bool   `json:"process_local,omitempty"`
	Held         bool   `json:"held,omitempty"`
	Epoch        int64  `json:"epoch,omitempty"`
}

// DumpCleanupWarning is the operator sentence for one leftover connection.
func DumpCleanupWarning(connectionID uint32) string {
	return fmt.Sprintf("source Binlog Dump connection %d may still be open; will KILL when source is reachable", connectionID)
}

// DumpCleanupProcessLocalWarning is DumpCleanupWarning plus the schema-3 limit.
// Cluster mode: only the worker that held the dump has this copy.
func DumpCleanupProcessLocalWarning(connectionID uint32) string {
	return DumpCleanupWarning(connectionID) + ". Only this process, the worker that held the dump, keeps this warning; another process or a restart does not, until migration 000004"
}

// DumpCleanupClearedMessage is the event text once that connection is gone.
func DumpCleanupClearedMessage(connectionID uint32) string {
	return fmt.Sprintf("source Binlog Dump connection %d closed", connectionID)
}

func (d DumpCleanup) warned() DumpCleanup {
	if d.ConnectionID == 0 || d.Held {
		d.Warning = ""
		return d
	}
	d.Warning = DumpCleanupWarning(d.ConnectionID)
	return d
}

func (d DumpCleanup) same(other DumpCleanup) bool {
	return d.ConnectionID == other.ConnectionID && d.Host == other.Host && d.Port == other.Port
}

// EncodeDumpCleanup stores host, port, connection id, and whether the owner
// still holds that dump. Empty means nothing pending.
func EncodeDumpCleanup(d DumpCleanup) string {
	if d.ConnectionID == 0 {
		return ""
	}
	raw, err := json.Marshal(struct {
		ConnectionID uint32 `json:"connection_id"`
		Host         string `json:"host,omitempty"`
		Port         uint16 `json:"port,omitempty"`
		Held         bool   `json:"held,omitempty"`
		Epoch        int64  `json:"epoch,omitempty"`
	}{d.ConnectionID, d.Host, d.Port, d.Held, d.Epoch})
	if err != nil {
		return ""
	}
	return string(raw)
}

// DecodeDumpCleanup reads a stored marker. A blank or broken value is nothing pending.
func DecodeDumpCleanup(raw string) DumpCleanup {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DumpCleanup{}
	}
	var d DumpCleanup
	if err := json.Unmarshal([]byte(raw), &d); err != nil || d.ConnectionID == 0 {
		return DumpCleanup{}
	}
	d.Warning = ""
	return d.warned()
}

// ApplyDumpCleanup is the pending-cleanup transition.
// A non-nil killErr means the source could not confirm the thread is gone.
// A nil killErr means KILL succeeded, the id is not a Binlog Dump anymore,
// or MySQL returned ER_NO_SUCH_THREAD.
// The same pending failure does not emit again. Success emits cleared once.
func ApplyDumpCleanup(current, attempted DumpCleanup, killErr error) (DumpCleanup, string) {
	if killErr != nil {
		if attempted.ConnectionID == 0 {
			return current, ""
		}
		attempted.Held = false
		if current.Epoch != 0 && attempted.Epoch == 0 {
			attempted.Epoch = current.Epoch
		}
		// A held connection that could not be killed becomes a leftover once.
		if current.same(attempted) && !current.Held {
			return current, ""
		}
		return attempted, dumpCleanupPending
	}
	if current.ConnectionID == 0 {
		return DumpCleanup{}, ""
	}
	if attempted.ConnectionID != 0 && current.ConnectionID != attempted.ConnectionID {
		return current, ""
	}
	// Closing the dump this worker still held is not a leftover event.
	if current.Held {
		return DumpCleanup{}, ""
	}
	return DumpCleanup{}, dumpCleanupCleared
}
