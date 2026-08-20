// Package replication provides module-level functionality for replication.
// input: source connection results, flavor, log_bin and identity variables
// output: stable source identity strings and permanent source configuration errors
// pos: flavor-aware source probe used before binlog dump starts
// note: if this file changes, update this header and module README.md.
package replication

import (
	"fmt"
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

// resolveSourceIdentity maps probed variables to a stable identity.
// MariaDB 11 has no @@server_uuid; identity is mariadb:<server_id>:<gtid_domain_id>.
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
		return "", tasks.NewPermanentError(tasks.CodeSourceIdentityUnavailable, "empty server_uuid")
	}
	return serverUUID, nil
}
