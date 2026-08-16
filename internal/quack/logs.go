package quack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// LogEntry is one row of the Quack message log. Fields are strings because the
// log's schema is registered by the extension and can gain columns between
// versions; anything unrecognised stays available in Raw for the detail panel.
type LogEntry struct {
	Timestamp    string
	MessageType  string
	ConnectionID string
	ClientQuery  string // client_query_id, for correlating client and server logs
	Query        string
	DurationMs   string
	ResponseType string
	Err          string
	Raw          map[string]interface{}
}

// Failed reports whether this entry recorded an error.
func (e LogEntry) Failed() bool {
	return e.Err != "" || strings.EqualFold(e.ResponseType, "ERROR")
}

// ParseLogRows converts duckdb_logs_parsed output into entries, newest first.
//
// Pintail's own log-reading query shows up in the log it is reading; those rows
// are dropped so the panel shows the server's traffic rather than the fact that
// it is being watched.
func ParseLogRows(data []byte) ([]LogEntry, error) {
	data = lastJSONArray(bytes.TrimSpace(data))
	if len(data) == 0 || string(data) == "[]" {
		return nil, nil
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}

	field := func(row map[string]interface{}, key string) string {
		if v, ok := row[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}

	entries := make([]LogEntry, 0, len(rows))
	for _, row := range rows {
		q := field(row, "query")
		if strings.Contains(q, "duckdb_logs_parsed") {
			continue // our own poll
		}
		entries = append(entries, LogEntry{
			Timestamp:    field(row, "timestamp"),
			MessageType:  field(row, "message_type"),
			ConnectionID: field(row, "quack_connection_id"),
			ClientQuery:  field(row, "client_query_id"),
			Query:        q,
			DurationMs:   field(row, "duration_ms"),
			ResponseType: field(row, "response_type"),
			Err:          field(row, "error"),
			Raw:          row,
		})
	}
	return entries, nil
}

// logSQL reads the most recent entries. SELECT * rather than named columns: the
// log's schema comes from the extension, so asking for a column it does not have
// would fail the whole fetch. Entries are filtered in Go rather than SQL for the
// same reason.
const logSQL = `SELECT * FROM duckdb_logs_parsed('Quack') ORDER BY timestamp DESC LIMIT 200`

// Logs reads the Quack message log from inside the server process.
//
// The log lives on the server, so this goes through quack_query the same way the
// session list does — reading duckdb_logs_parsed through our own attached
// session would report on the CLI we just started instead.
func (c *QuackClient) Logs(ctx context.Context) ([]LogEntry, error) {
	if !c.hasCLI {
		return nil, fmt.Errorf("duckdb CLI not found in PATH")
	}
	out, err := c.serverInvocation(logSQL, "-json").command(ctx, c.cliPath).Output()
	if err != nil {
		return nil, fmt.Errorf("%s", cliError(err))
	}
	return ParseLogRows(out)
}

// EnableLogging turns Quack logging on for the server.
func (c *QuackClient) EnableLogging(ctx context.Context) error {
	return c.RunServerSQL(ctx, "CALL enable_logging('Quack')")
}
