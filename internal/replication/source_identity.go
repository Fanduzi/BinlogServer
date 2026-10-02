// Package replication provides module-level functionality for replication.
// input: source connection results, network/stream disconnects, flavor, log_bin and identity variables
// output: stable source identity strings, MariaDB flavor hint when @@server_uuid is missing, and typed permanent/retryable source errors
// pos: flavor-aware source probe and operator-error classification boundary
// note: if this file changes, update this header and module README.md.
package replication

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"binlog_server/internal/tasks"
)

func isAccessDeniedMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(msg, "ERROR 1045") ||
		strings.Contains(msg, "Error 1045") ||
		strings.Contains(lower, "access denied")
}

func classifySourceError(err error) error {
	if err == nil {
		return nil
	}
	if tasks.IsPermanent(err) {
		return err
	}
	if isAccessDeniedMessage(err.Error()) {
		return tasks.NewPermanentError(tasks.CodeSourceAccessDenied, err.Error())
	}
	var networkErr *net.OpError
	if errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return tasks.NewRetryableSourceError(tasks.CodeSourceUnreachable, err.Error())
	}
	return err
}

func isLogBinEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "1", "true":
		return true
	default:
		return false
	}
}

func isMariaDBFlavor(flavor string) bool {
	return strings.EqualFold(strings.TrimSpace(flavor), "mariadb")
}

// mysqlServerUUIDUnavailable is the operator message when flavor=mysql probes a
// source that has no @@server_uuid. MariaDB returns an empty SHOW VARIABLES row
// or ERROR 1193 (unknown system variable).
const mysqlServerUUIDUnavailable = "server_uuid is unavailable (empty result or unknown system variable). This source looks like MariaDB. Set flavor=mariadb"

func isUnknownSystemVariable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown system variable") || strings.Contains(msg, "error 1193")
}

// identityFromProbe maps a server_uuid probe onto resolveSourceIdentity.
// ERROR 1193 and an empty result are the same missing-variable signal.
func identityFromProbe(flavor, logBin, serverUUID string, uuidErr error, serverID, domainID string) (string, error) {
	if isUnknownSystemVariable(uuidErr) {
		serverUUID = ""
		uuidErr = nil
	}
	if uuidErr != nil {
		return "", uuidErr
	}
	return resolveSourceIdentity(flavor, logBin, serverUUID, serverID, domainID)
}

// resolveSourceIdentity maps probed variables to a stable identity.
// MariaDB has no @@server_uuid; identity is mariadb:<server_id>:<gtid_domain_id>.
func resolveSourceIdentity(flavor, logBin, serverUUID, serverID, domainID string) (string, error) {
	if !isLogBinEnabled(logBin) {
		return "", tasks.NewPermanentError(tasks.CodeSourceLogBinOff, "log_bin is off")
	}
	if isMariaDBFlavor(flavor) {
		serverID = strings.TrimSpace(serverID)
		if serverID == "" {
			return "", tasks.NewPermanentError(tasks.CodeSourceIdentityUnavailable, "empty mariadb server_id")
		}
		domainID = strings.TrimSpace(domainID)
		if domainID == "" {
			domainID = "0"
		}
		return fmt.Sprintf("mariadb:%s:%s", serverID, domainID), nil
	}
	serverUUID = strings.TrimSpace(serverUUID)
	if serverUUID == "" {
		return "", tasks.NewPermanentError(tasks.CodeSourceIdentityUnavailable, mysqlServerUUIDUnavailable)
	}
	return serverUUID, nil
}
