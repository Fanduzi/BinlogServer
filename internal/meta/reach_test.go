package meta

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

// TestIsUnreachable (#427, design §25.5): a store that answered with an SQL
// error is reachable; a timeout or a connection failure is not.
func TestIsUnreachable(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"read-only 1290", &mysqlDriver.MySQLError{Number: 1290, Message: "read-only"}, false},
		{"access denied 1045", &mysqlDriver.MySQLError{Number: 1045}, false},
		{"missing table 1146", fmt.Errorf("list: %w", &mysqlDriver.MySQLError{Number: 1146}), false},
		{"can't connect 2003", &mysqlDriver.MySQLError{Number: 2003}, true},
		{"gone away 2006", &mysqlDriver.MySQLError{Number: 2006}, true},
		{"lost 2013", &mysqlDriver.MySQLError{Number: 2013}, true},
		{"deadline", fmt.Errorf("list open: %w", context.DeadlineExceeded), true},
		{"bad conn", driver.ErrBadConn, true},
		{"invalid conn", mysqlDriver.ErrInvalidConn, true},
		{"refused", fmt.Errorf("ping: %w", refused), true},
		{"plain", errors.New("syntax error"), false},
	}
	for _, c := range cases {
		if got := IsUnreachable(c.err); got != c.want {
			t.Errorf("%s: IsUnreachable(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
