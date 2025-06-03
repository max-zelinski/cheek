package cheek

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	_ "github.com/glebarez/go-sqlite"
)

// LogLine represents a single line of log output from a job run
type LogLine struct {
	ID         int    `json:"id" db:"id"`
	JobRunID   int    `json:"job_run_id" db:"job_run_id"`
	LineNumber int    `json:"line_number" db:"line_number"`
	Timestamp  string `json:"timestamp" db:"timestamp"`
	Content    string `json:"content" db:"content"`
	Stream     string `json:"stream" db:"stream"`
}

func OpenDB(dbPath string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := InitDB(db); err != nil {
		return nil, fmt.Errorf("init db: %w", err)
	}

	return db, nil
}

func InitDB(db *sqlx.DB) error {
	// Create the log table if it doesn't exist
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS log (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        job TEXT,
        triggered_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		triggered_by TEXT,
        duration INTEGER,
        status INTEGER,
        message TEXT,
		is_running INTEGER DEFAULT 0,
		UNIQUE(job, triggered_at, triggered_by)
    )`)
	if err != nil {
		return fmt.Errorf("create log table: %w", err)
	}

	// Add is_running column to existing log table if it doesn't exist
	_, err = db.Exec(`ALTER TABLE log ADD COLUMN is_running INTEGER DEFAULT 0`)
	if err != nil {
		// Ignore error if column already exists
		// SQLite doesn't have a clean way to check if column exists
	}

	// Create the log_lines table
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS log_lines (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_run_id INTEGER NOT NULL,
		line_number INTEGER NOT NULL,
		timestamp TEXT NOT NULL,
		content TEXT NOT NULL,
		stream TEXT NOT NULL,
		FOREIGN KEY (job_run_id) REFERENCES log(id),
		UNIQUE(job_run_id, line_number)
	)`)
	if err != nil {
		return fmt.Errorf("create log_lines table: %w", err)
	}

	// Create index for efficient queries
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_log_lines_job_run_id ON log_lines(job_run_id)`)
	if err != nil {
		return fmt.Errorf("create log_lines index: %w", err)
	}

	// Perform cleanup to remove old, non-conforming records
	_, err = db.Exec(`
		DELETE FROM log
		WHERE id NOT IN (
			SELECT MIN(id)
			FROM log
			GROUP BY job, triggered_at, triggered_by
		);
	`)
	if err != nil {
		return fmt.Errorf("cleanup old log records: %w", err)
	}

	return nil
}

// InsertLogLine inserts a single log line
func InsertLogLine(db *sqlx.DB, jobRunID int, lineNumber int, content string, stream string) error {
	_, err := db.Exec(`
		INSERT INTO log_lines (job_run_id, line_number, timestamp, content, stream) 
		VALUES (?, ?, ?, ?, ?)`,
		jobRunID, lineNumber, time.Now().Format(time.RFC3339), content, stream)
	if err != nil {
		return fmt.Errorf("insert log line: %w", err)
	}
	return nil
}

// GetLogLines retrieves log lines for a job run, optionally after a specific line number
func GetLogLines(db *sqlx.DB, jobRunID int, afterLineNumber int) ([]LogLine, error) {
	var lines []LogLine
	query := `
		SELECT id, job_run_id, line_number, timestamp, content, stream 
		FROM log_lines 
		WHERE job_run_id = ? AND line_number > ?
		ORDER BY line_number ASC`

	err := db.Select(&lines, query, jobRunID, afterLineNumber)
	if err != nil {
		return nil, fmt.Errorf("get log lines: %w", err)
	}
	return lines, nil
}

// InsertOrUpdateJobRun inserts a new job run or updates an existing one
func InsertOrUpdateJobRun(db *sqlx.DB, jr *JobRun) error {
	// Determine is_running status
	isRunning := 0
	if jr.Status == nil {
		isRunning = 1 // Job is still running if status is nil
	}

	// Perform an UPSERT (insert or update)
	result, err := db.Exec(`
		INSERT INTO log (job, triggered_at, triggered_by, duration, status, message, is_running) 
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job, triggered_at, triggered_by) DO UPDATE SET 
			duration = excluded.duration, 
			status = excluded.status, 
			message = excluded.message,
			is_running = excluded.is_running`,
		jr.Name, jr.TriggeredAt, jr.TriggeredBy, jr.Duration, jr.Status, jr.Log, isRunning)
	if err != nil {
		return fmt.Errorf("insert or update job run: %w", err)
	}

	// Try to get the ID from the result if we don't have it yet
	if jr.LogEntryId == 0 {
		lastId, err := result.LastInsertId()
		if err == nil && lastId > 0 {
			jr.LogEntryId = int(lastId)
		}

		// If LastInsertId doesn't work, query for the ID
		if jr.LogEntryId == 0 {
			err = db.Get(&jr.LogEntryId,
				"SELECT id FROM log WHERE job = ? AND triggered_at = ? AND triggered_by = ?",
				jr.Name, jr.TriggeredAt, jr.TriggeredBy)
			if err != nil {
				return fmt.Errorf("get job run ID: %w", err)
			}
		}
	}

	return nil
}

// LoadJobRun loads a single job run by ID, or the latest run if id is -1
func LoadJobRun(db *sqlx.DB, jobName string, id int) (JobRun, error) {
	var jr JobRun

	// if id -1 then load last run
	if id == -1 {
		err := db.Get(&jr, "SELECT id, triggered_at, triggered_by, duration, status, message FROM log WHERE job = ? ORDER BY triggered_at DESC LIMIT 1", jobName)
		if err != nil {
			return jr, fmt.Errorf("load latest job run: %w", err)
		}
		return jr, nil
	}

	err := db.Get(&jr, "SELECT id, triggered_at, triggered_by, duration, status, message FROM log WHERE id = ?", id)
	if err != nil {
		return jr, fmt.Errorf("load job run by id: %w", err)
	}
	return jr, nil
}

// LoadJobRuns loads multiple job runs for a specific job
func LoadJobRuns(db *sqlx.DB, jobName string, nruns int, includeLogs bool) ([]JobRun, error) {
	var query string
	if includeLogs {
		query = "SELECT id, triggered_at, triggered_by, duration, status, message FROM log WHERE job = ? ORDER BY triggered_at DESC LIMIT ?"
	} else {
		query = "SELECT id, triggered_at, triggered_by, duration, status FROM log WHERE job = ? ORDER BY triggered_at DESC LIMIT ?"
	}

	var jrs []JobRun
	err := db.Select(&jrs, query, jobName, nruns)
	if err != nil {
		return nil, fmt.Errorf("load job runs: %w", err)
	}
	return jrs, nil
}
