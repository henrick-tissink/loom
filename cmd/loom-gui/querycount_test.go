package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
)

// A statement counter, for the bindings whose contract is a COUNT rather than a
// value.
//
// Orchestrator §13 binds: "ListProjectDetails issues one orchestrator query for
// N projects (no N+1)". That is not assertable from the returned DTO — an N+1
// and a single query produce identical output — so it is asserted from
// underneath, by wrapping modernc's driver and counting the statements that
// reach SQLite.
//
// The rejected alternative was to assert it by reading the source (a loop with
// no store call inside it), which passes the day it is written and says nothing
// the day someone adds a helper that queries.
type countingDriver struct{ base driver.Driver }

var (
	stmtMu  sync.Mutex
	stmtLog []string
)

func init() {
	// The driver value is borrowed from a throwaway *sql.DB rather than
	// imported: modernc.org/sqlite registers itself under "sqlite" and does not
	// export its driver type, and the house rule forbids adding a dependency to
	// reach one.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	sql.Register("sqlite-counting", countingDriver{base: db.Driver()})
	_ = db.Close()
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: c}, nil
}

type countingConn struct{ driver.Conn }

// Counted ONCE per statement. Query/ExecContext count only when they actually
// delegate: returning driver.ErrSkip sends database/sql back through Prepare,
// and counting both would double every statement.
func note(q string) {
	stmtMu.Lock()
	stmtLog = append(stmtLog, q)
	stmtMu.Unlock()
}

func (c *countingConn) Prepare(q string) (driver.Stmt, error) {
	note(q)
	return c.Conn.Prepare(q)
}

func (c *countingConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	note(q)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, q)
	}
	return c.Conn.Prepare(q)
}

func (c *countingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	note(q)
	return qc.QueryContext(ctx, q, args)
}

func (c *countingConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	note(q)
	return ec.ExecContext(ctx, q, args)
}

// countStatements runs f and returns every statement that reached SQLite,
// matching sub (case-insensitively) — the table name, in practice.
func countStatements(sub string, f func()) []string {
	stmtMu.Lock()
	stmtLog = nil
	stmtMu.Unlock()

	f()

	stmtMu.Lock()
	defer stmtMu.Unlock()
	var hits []string
	for _, q := range stmtLog {
		if strings.Contains(strings.ToLower(q), strings.ToLower(sub)) {
			hits = append(hits, q)
		}
	}
	return hits
}
