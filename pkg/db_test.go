package cheek

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3" // Import SQLite driver for database/sql
	"github.com/stretchr/testify/assert"
)

// setupTestDB creates an in-memory SQLite database for testing
func setupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open in-memory database: %v", err)
	}

	// Initialize database
	err = InitDB(db)
	if err != nil {
		db.Close()
		t.Fatalf("Failed to initialize database: %v", err)
	}

	return db
}

// TestInitDB tests the InitDB function, including the cleanup logic.
func TestInitDB(t *testing.T) {
	// Create an in-memory SQLite database
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open in-memory database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Create the log table without the UNIQUE constraint temporarily
	_, err = db.Exec(`CREATE TABLE log_temp (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        job TEXT,
        triggered_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		triggered_by TEXT,
        duration INTEGER,
        status INTEGER,
        message TEXT
    )`)
	if err != nil {
		t.Fatalf("Failed to create temporary log table: %v", err)
	}

	// Insert conflicting data into the temporary log table
	_, err = db.Exec(`
		INSERT INTO log_temp (job, triggered_at, triggered_by, duration, status, message) VALUES
		('job1', '2023-10-01 10:00:00', 'user1', 120, 1, 'Success'),
		('job1', '2023-10-01 10:00:00', 'user1', 150, 1, 'Success'),  -- Duplicate
		('job1', '2023-10-01 11:00:00', 'user2', 90, 0, 'Failed')
	`)
	if err != nil {
		t.Fatalf("Failed to insert test data: %v", err)
	}

	// Create the actual log table with the UNIQUE constraint
	_, err = db.Exec(`CREATE TABLE log (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        job TEXT,
        triggered_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		triggered_by TEXT,
        duration INTEGER,
        status INTEGER,
        message TEXT,
		UNIQUE(job, triggered_at, triggered_by)
    )`)
	if err != nil {
		t.Fatalf("Failed to create log table: %v", err)
	}

	// Move data from the temporary table to the log table
	_, err = db.Exec(`
		INSERT INTO log (job, triggered_at, triggered_by, duration, status, message)
		SELECT job, triggered_at, triggered_by, duration, status, message 
		FROM log_temp
		WHERE true
		ON CONFLICT (job, triggered_at, triggered_by) DO NOTHING
	`)

	if err != nil {
		t.Fatalf("Failed to transfer data to log table: %v", err)
	}

	//Drop the temporary table
	_, err = db.Exec("DROP TABLE log_temp;")
	if err != nil {
		t.Fatalf("Failed to drop temporary log table: %v", err)
	}

	// Call the InitDB function
	err = InitDB(db)
	assert.NoError(t, err, "InitDB should not return an error")

	// Check if cleanup worked correctly
	var cleanedCount int
	err = db.Get(&cleanedCount, "SELECT COUNT(*) FROM log")
	assert.NoError(t, err, "Querying the log table should not return an error")
	assert.Equal(t, 2, cleanedCount, "There should be 2 unique records in the log table after cleanup")

}

// TestLogLinesTable tests the creation of the log_lines table
func TestLogLinesTable(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Check if log_lines table exists
	var tableExists int
	err := db.Get(&tableExists, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='log_lines'")
	assert.NoError(t, err, "Checking for log_lines table should not return an error")
	assert.Equal(t, 1, tableExists, "log_lines table should exist")

	// Check if index exists
	var indexExists int
	err = db.Get(&indexExists, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_log_lines_job_run_id'")
	assert.NoError(t, err, "Checking for index should not return an error")
	assert.Equal(t, 1, indexExists, "Index on job_run_id should exist")
}

// TestInsertLogLine tests the InsertLogLine method
func TestInsertLogLine(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert a job run first
	result, err := db.Exec(`INSERT INTO log (job, triggered_at, triggered_by) VALUES (?, ?, ?)`,
		"test_job", "2023-10-01 10:00:00", "test")
	assert.NoError(t, err, "Inserting job run should not return an error")

	jobRunID, err := result.LastInsertId()
	assert.NoError(t, err, "Getting last insert ID should not return an error")

	// Test inserting log lines
	err = InsertLogLine(db, int(jobRunID), 1, "First line of output", "stdout")
	assert.NoError(t, err, "InsertLogLine should not return an error")

	err = InsertLogLine(db, int(jobRunID), 2, "Second line of output", "stdout")
	assert.NoError(t, err, "InsertLogLine should not return an error")

	err = InsertLogLine(db, int(jobRunID), 3, "Error message", "stderr")
	assert.NoError(t, err, "InsertLogLine should not return an error")

	// Verify lines were inserted
	var count int
	err = db.Get(&count, "SELECT COUNT(*) FROM log_lines WHERE job_run_id = ?", jobRunID)
	assert.NoError(t, err, "Counting log lines should not return an error")
	assert.Equal(t, 3, count, "Should have 3 log lines")

	// Test duplicate line number (should fail due to UNIQUE constraint)
	err = InsertLogLine(db, int(jobRunID), 1, "Duplicate line", "stdout")
	assert.Error(t, err, "Inserting duplicate line number should return an error")
}

// TestGetLogLines tests the GetLogLines method
func TestGetLogLines(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert a job run
	result, err := db.Exec(`INSERT INTO log (job, triggered_at, triggered_by) VALUES (?, ?, ?)`,
		"test_job", "2023-10-01 10:00:00", "test")
	assert.NoError(t, err, "Inserting job run should not return an error")

	jobRunID, err := result.LastInsertId()
	assert.NoError(t, err, "Getting last insert ID should not return an error")

	// Insert multiple log lines
	lines := []struct {
		lineNum int
		content string
		stream  string
	}{
		{1, "Starting job", "stdout"},
		{2, "Processing data", "stdout"},
		{3, "Warning: low memory", "stderr"},
		{4, "Job completed", "stdout"},
	}

	for _, line := range lines {
		err = InsertLogLine(db, int(jobRunID), line.lineNum, line.content, line.stream)
		assert.NoError(t, err, "InsertLogLine should not return an error")
	}

	// Test getting all lines
	logLines, err := GetLogLines(db, int(jobRunID), 0)
	assert.NoError(t, err, "GetLogLines should not return an error")
	assert.Len(t, logLines, 4, "Should return 4 log lines")

	// Verify order
	for i, line := range logLines {
		assert.Equal(t, i+1, line.LineNumber, "Lines should be in order")
		assert.Equal(t, lines[i].content, line.Content, "Content should match")
		assert.Equal(t, lines[i].stream, line.Stream, "Stream should match")
	}

	// Test getting lines after a specific line number
	logLines, err = GetLogLines(db, int(jobRunID), 2)
	assert.NoError(t, err, "GetLogLines with afterLineNumber should not return an error")
	assert.Len(t, logLines, 2, "Should return 2 log lines after line 2")
	assert.Equal(t, 3, logLines[0].LineNumber, "First line should be line 3")
	assert.Equal(t, 4, logLines[1].LineNumber, "Second line should be line 4")

	// Test with non-existent job run ID
	logLines, err = GetLogLines(db, 9999, 0)
	assert.NoError(t, err, "GetLogLines with non-existent job run should not return an error")
	assert.Len(t, logLines, 0, "Should return empty slice for non-existent job run")
}

// TestInsertOrUpdateJobRun tests the InsertOrUpdateJobRun function
func TestInsertOrUpdateJobRun(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create a test JobRun
	now := time.Now()
	jr := &JobRun{
		Name:        "test_job",
		TriggeredAt: now,
		TriggeredBy: "manual",
		Status:      nil, // Running job
		Log:         "Job is starting...",
		Duration:    0,
	}

	// Test inserting a new job run
	err := InsertOrUpdateJobRun(db, jr)
	assert.NoError(t, err, "InsertOrUpdateJobRun should not return an error")
	assert.NotZero(t, jr.LogEntryId, "JobRun should have a LogEntryId after insert")

	// Verify the job was inserted with is_running = 1
	var count int
	var isRunning int
	err = db.Get(&count, "SELECT COUNT(*) FROM log WHERE job = ?", jr.Name)
	assert.NoError(t, err)
	assert.Equal(t, 1, count, "Should have one log entry")

	err = db.Get(&isRunning, "SELECT is_running FROM log WHERE id = ?", jr.LogEntryId)
	assert.NoError(t, err)
	assert.Equal(t, 1, isRunning, "Job should be marked as running")

	// Update the job run (job completed)
	exitStatus := 0
	jr.Status = &exitStatus
	jr.Log = "Job is starting...\nJob completed successfully"
	jr.Duration = 5000

	err = InsertOrUpdateJobRun(db, jr)
	assert.NoError(t, err, "UpdateJobRun should not return an error")

	// Verify the job was updated with is_running = 0
	err = db.Get(&isRunning, "SELECT is_running FROM log WHERE id = ?", jr.LogEntryId)
	assert.NoError(t, err)
	assert.Equal(t, 0, isRunning, "Job should be marked as not running")

	// Verify other fields were updated
	var duration int
	var status int
	var message string
	err = db.QueryRow("SELECT duration, status, message FROM log WHERE id = ?", jr.LogEntryId).Scan(&duration, &status, &message)
	assert.NoError(t, err)
	assert.Equal(t, 5000, int(jr.Duration), "Duration should be updated")
	assert.Equal(t, 0, status, "Status should be updated")
	assert.Equal(t, "Job is starting...\nJob completed successfully", message, "Message should be updated")

	// Test that count remains 1 (update, not insert)
	err = db.Get(&count, "SELECT COUNT(*) FROM log WHERE job = ?", jr.Name)
	assert.NoError(t, err)
	assert.Equal(t, 1, count, "Should still have only one log entry")
}

// TestInsertOrUpdateJobRunMultipleRuns tests multiple runs of the same job
func TestInsertOrUpdateJobRunMultipleRuns(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create first job run
	now1 := time.Now()
	jr1 := &JobRun{
		Name:        "test_job",
		TriggeredAt: now1,
		TriggeredBy: "cron",
		Status:      nil,
		Log:         "First run starting...",
		Duration:    0,
	}

	err := InsertOrUpdateJobRun(db, jr1)
	assert.NoError(t, err, "First insert should not return an error")
	id1 := jr1.LogEntryId

	// Create second job run (different trigger time)
	now2 := now1.Add(time.Minute)
	jr2 := &JobRun{
		Name:        "test_job",
		TriggeredAt: now2,
		TriggeredBy: "manual",
		Status:      nil,
		Log:         "Second run starting...",
		Duration:    0,
	}

	err = InsertOrUpdateJobRun(db, jr2)
	assert.NoError(t, err, "Second insert should not return an error")
	id2 := jr2.LogEntryId

	// Verify we have two separate entries
	assert.NotEqual(t, id1, id2, "Should have different IDs")

	var count int
	err = db.Get(&count, "SELECT COUNT(*) FROM log WHERE job = ?", "test_job")
	assert.NoError(t, err)
	assert.Equal(t, 2, count, "Should have two log entries")

	// Complete first job
	exitStatus1 := 0
	jr1.Status = &exitStatus1
	jr1.Log = "First run starting...\nFirst run completed"
	jr1.Duration = 3000

	err = InsertOrUpdateJobRun(db, jr1)
	assert.NoError(t, err, "Updating first job should not return an error")

	// Complete second job with error
	exitStatus2 := 1
	jr2.Status = &exitStatus2
	jr2.Log = "Second run starting...\nSecond run failed"
	jr2.Duration = 1500

	err = InsertOrUpdateJobRun(db, jr2)
	assert.NoError(t, err, "Updating second job should not return an error")

	// Verify both jobs are marked as not running
	var isRunning1, isRunning2 int
	err = db.Get(&isRunning1, "SELECT is_running FROM log WHERE id = ?", id1)
	assert.NoError(t, err)
	assert.Equal(t, 0, isRunning1, "First job should not be running")

	err = db.Get(&isRunning2, "SELECT is_running FROM log WHERE id = ?", id2)
	assert.NoError(t, err)
	assert.Equal(t, 0, isRunning2, "Second job should not be running")

	// Verify final states
	var status1, status2 int
	err = db.Get(&status1, "SELECT status FROM log WHERE id = ?", id1)
	assert.NoError(t, err)
	assert.Equal(t, 0, status1, "First job should have status 0")

	err = db.Get(&status2, "SELECT status FROM log WHERE id = ?", id2)
	assert.NoError(t, err)
	assert.Equal(t, 1, status2, "Second job should have status 1")

	// Total count should still be 2
	err = db.Get(&count, "SELECT COUNT(*) FROM log WHERE job = ?", "test_job")
	assert.NoError(t, err)
	assert.Equal(t, 2, count, "Should still have two log entries")
}

// TestJobRunDatabaseIntegration tests the integration between JobRun and database functions
func TestJobRunDatabaseIntegration(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Step 1: Create and insert a new job run
	now := time.Now()
	jr := &JobRun{
		Name:        "integration_test",
		TriggeredAt: now,
		TriggeredBy: "manual",
		Status:      nil, // Running job
		Log:         "Job started",
		Duration:    0,
	}

	// Insert the job run
	err := InsertOrUpdateJobRun(db, jr)
	assert.NoError(t, err, "Should insert job run successfully")
	assert.NotZero(t, jr.LogEntryId, "Should have LogEntryId after insert")
	originalID := jr.LogEntryId

	// Step 2: Add log lines while job is running
	err = InsertLogLine(db, jr.LogEntryId, 1, "Processing step 1...", "stdout")
	assert.NoError(t, err)
	err = InsertLogLine(db, jr.LogEntryId, 2, "Processing step 2...", "stdout")
	assert.NoError(t, err)
	err = InsertLogLine(db, jr.LogEntryId, 3, "Warning: resource usage high", "stderr")
	assert.NoError(t, err)

	// Step 3: Load the job run using LoadJobRun
	loadedJr, err := LoadJobRun(db, "integration_test", originalID)
	assert.NoError(t, err, "Should load job run successfully")
	assert.Equal(t, originalID, loadedJr.LogEntryId)
	assert.Equal(t, "manual", loadedJr.TriggeredBy)
	assert.Nil(t, loadedJr.Status, "Job should still be running")

	// Step 4: Get log lines for the running job
	logLines, err := GetLogLines(db, jr.LogEntryId, 0)
	assert.NoError(t, err)
	assert.Len(t, logLines, 3, "Should have 3 log lines")
	assert.Equal(t, "Processing step 1...", logLines[0].Content)
	assert.Equal(t, "Warning: resource usage high", logLines[2].Content)
	assert.Equal(t, "stderr", logLines[2].Stream)

	// Step 5: Complete the job
	exitStatus := 0
	jr.Status = &exitStatus
	jr.Log = "Job started\nProcessing step 1...\nProcessing step 2...\nWarning: resource usage high\nJob completed successfully"
	jr.Duration = 2500

	err = InsertOrUpdateJobRun(db, jr)
	assert.NoError(t, err, "Should update job run successfully")

	// Step 6: Add more log lines after completion
	err = InsertLogLine(db, jr.LogEntryId, 4, "Job completed successfully", "stdout")
	assert.NoError(t, err)

	// Step 7: Create another job run for the same job
	now2 := now.Add(time.Hour)
	jr2 := &JobRun{
		Name:        "integration_test",
		TriggeredAt: now2,
		TriggeredBy: "cron",
		Status:      nil,
		Log:         "Second run started",
		Duration:    0,
	}

	err = InsertOrUpdateJobRun(db, jr2)
	assert.NoError(t, err)

	// Complete second job with error
	exitStatus2 := 1
	jr2.Status = &exitStatus2
	jr2.Log = "Second run started\nError occurred"
	jr2.Duration = 1000

	err = InsertOrUpdateJobRun(db, jr2)
	assert.NoError(t, err)

	// Step 8: Test LoadJobRun with -1 to get latest
	latestJr, err := LoadJobRun(db, "integration_test", -1)
	assert.NoError(t, err)
	assert.Equal(t, jr2.LogEntryId, latestJr.LogEntryId, "Should load the most recent job run")
	assert.Equal(t, "cron", latestJr.TriggeredBy)
	assert.Equal(t, 1, *latestJr.Status)

	// Step 9: Test LoadJobRuns to get all runs
	allRuns, err := LoadJobRuns(db, "integration_test", 10, true)
	assert.NoError(t, err)
	assert.Len(t, allRuns, 2, "Should have 2 job runs")
	assert.Equal(t, "cron", allRuns[0].TriggeredBy, "Latest run should be first")
	assert.Equal(t, "manual", allRuns[1].TriggeredBy, "First run should be second")
	assert.Equal(t, "Second run started\nError occurred", allRuns[0].Log)

	// Step 10: Test pagination with GetLogLines
	newLines, err := GetLogLines(db, originalID, 2)
	assert.NoError(t, err)
	assert.Len(t, newLines, 2, "Should get lines after line 2")
	assert.Equal(t, 3, newLines[0].LineNumber)
	assert.Equal(t, 4, newLines[1].LineNumber)

	// Step 11: Verify is_running status for both jobs
	var isRunning1, isRunning2 int
	err = db.Get(&isRunning1, "SELECT is_running FROM log WHERE id = ?", originalID)
	assert.NoError(t, err)
	assert.Equal(t, 0, isRunning1, "First job should not be running")

	err = db.Get(&isRunning2, "SELECT is_running FROM log WHERE id = ?", jr2.LogEntryId)
	assert.NoError(t, err)
	assert.Equal(t, 0, isRunning2, "Second job should not be running")

	// Step 12: Test LoadJobRuns without logs
	runsWithoutLogs, err := LoadJobRuns(db, "integration_test", 10, false)
	assert.NoError(t, err)
	assert.Len(t, runsWithoutLogs, 2)
	assert.Empty(t, runsWithoutLogs[0].Log, "Should not include logs when includeLogs=false")
	assert.Empty(t, runsWithoutLogs[1].Log, "Should not include logs when includeLogs=false")
}

// TestLoadJobRun tests the LoadJobRun function
func TestLoadJobRun(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert test job runs
	now := time.Now()

	// Insert first job run
	_, err := db.Exec(`INSERT INTO log (job, triggered_at, triggered_by, duration, status, message) VALUES (?, ?, ?, ?, ?, ?)`,
		"test_job", now.Format("2006-01-02 15:04:05"), "manual", 1000, 0, "Job completed successfully")
	assert.NoError(t, err, "Should insert first job run")

	// Insert second job run (more recent)
	now2 := now.Add(time.Hour)
	_, err = db.Exec(`INSERT INTO log (job, triggered_at, triggered_by, duration, status, message) VALUES (?, ?, ?, ?, ?, ?)`,
		"test_job", now2.Format("2006-01-02 15:04:05"), "cron", 2000, 1, "Job failed")
	assert.NoError(t, err, "Should insert second job run")

	// Get the second job run ID (should be 2)
	var secondJobID int
	err = db.Get(&secondJobID, "SELECT id FROM log WHERE job = ? ORDER BY triggered_at DESC LIMIT 1", "test_job")
	assert.NoError(t, err)

	// Test loading latest job run (id = -1)
	jr, err := LoadJobRun(db, "test_job", -1)
	assert.NoError(t, err, "Should load latest job run successfully")
	assert.Equal(t, secondJobID, jr.LogEntryId, "Should load the most recent job run")
	assert.Equal(t, "cron", jr.TriggeredBy, "Should match the most recent job run")
	assert.Equal(t, 1, *jr.Status, "Should match the most recent job run status")
	assert.Equal(t, "Job failed", jr.Log, "Should match the most recent job run message")

	// Test loading specific job run by ID
	jr, err = LoadJobRun(db, "test_job", 1)
	assert.NoError(t, err, "Should load specific job run successfully")
	assert.Equal(t, 1, jr.LogEntryId, "Should load the specified job run")
	assert.Equal(t, "manual", jr.TriggeredBy, "Should match the specified job run")
	assert.Equal(t, 0, *jr.Status, "Should match the specified job run status")
	assert.Equal(t, "Job completed successfully", jr.Log, "Should match the specified job run message")

	// Test loading non-existent job run
	_, err = LoadJobRun(db, "nonexistent_job", -1)
	assert.Error(t, err, "Should return error for non-existent job")

	// Test loading non-existent job run by ID
	_, err = LoadJobRun(db, "test_job", 9999)
	assert.Error(t, err, "Should return error for non-existent job run ID")
}

// TestLoadJobRuns tests the LoadJobRuns function
func TestLoadJobRuns(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert multiple job runs for testing
	jobName := "test_job"
	now := time.Now()

	jobRuns := []struct {
		triggeredAt time.Time
		triggeredBy string
		duration    int
		status      int
		message     string
	}{
		{now.Add(-3 * time.Hour), "cron", 1000, 0, "First job completed"},
		{now.Add(-2 * time.Hour), "manual", 1500, 1, "Second job failed"},
		{now.Add(-1 * time.Hour), "cron", 2000, 0, "Third job completed"},
		{now, "webhook", 500, 0, "Latest job completed"},
	}

	for _, jr := range jobRuns {
		_, err := db.Exec(`INSERT INTO log (job, triggered_at, triggered_by, duration, status, message) VALUES (?, ?, ?, ?, ?, ?)`,
			jobName, jr.triggeredAt.Format("2006-01-02 15:04:05"), jr.triggeredBy, jr.duration, jr.status, jr.message)
		assert.NoError(t, err, "Should insert job run")
	}

	// Test loading all job runs with logs
	jrs, err := LoadJobRuns(db, jobName, 10, true)
	assert.NoError(t, err, "Should load job runs successfully")
	assert.Len(t, jrs, 4, "Should return all 4 job runs")

	// Verify they are in reverse chronological order (latest first)
	assert.Equal(t, "webhook", jrs[0].TriggeredBy, "Latest job should be first")
	assert.Equal(t, "cron", jrs[1].TriggeredBy, "Third job should be second")
	assert.Equal(t, "manual", jrs[2].TriggeredBy, "Second job should be third")
	assert.Equal(t, "cron", jrs[3].TriggeredBy, "First job should be last")

	// Verify log messages are included
	assert.Equal(t, "Latest job completed", jrs[0].Log, "Should include log message")
	assert.Equal(t, "Third job completed", jrs[1].Log, "Should include log message")

	// Test loading limited number of job runs with logs
	jrs, err = LoadJobRuns(db, jobName, 2, true)
	assert.NoError(t, err, "Should load limited job runs successfully")
	assert.Len(t, jrs, 2, "Should return only 2 job runs")
	assert.Equal(t, "webhook", jrs[0].TriggeredBy, "Should be the latest job")
	assert.Equal(t, "cron", jrs[1].TriggeredBy, "Should be the second latest job")

	// Test loading job runs without logs
	jrs, err = LoadJobRuns(db, jobName, 3, false)
	assert.NoError(t, err, "Should load job runs without logs successfully")
	assert.Len(t, jrs, 3, "Should return 3 job runs")

	// Verify log messages are not included (should be empty)
	assert.Empty(t, jrs[0].Log, "Should not include log message when includeLogs=false")
	assert.Empty(t, jrs[1].Log, "Should not include log message when includeLogs=false")
	assert.Empty(t, jrs[2].Log, "Should not include log message when includeLogs=false")

	// Test loading job runs for non-existent job
	jrs, err = LoadJobRuns(db, "nonexistent_job", 10, true)
	assert.NoError(t, err, "Should not return error for non-existent job")
	assert.Len(t, jrs, 0, "Should return empty slice for non-existent job")
}

// TestLoadJobRunsMultipleJobs tests LoadJobRuns with multiple different jobs
func TestLoadJobRunsMultipleJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	now := time.Now()

	// Insert job runs for different jobs
	jobs := []struct {
		name        string
		triggeredBy string
		message     string
	}{
		{"job_a", "manual", "Job A completed"},
		{"job_b", "cron", "Job B completed"},
		{"job_a", "webhook", "Job A second run"},
		{"job_c", "manual", "Job C completed"},
		{"job_b", "manual", "Job B second run"},
	}

	for i, job := range jobs {
		_, err := db.Exec(`INSERT INTO log (job, triggered_at, triggered_by, duration, status, message) VALUES (?, ?, ?, ?, ?, ?)`,
			job.name, now.Add(time.Duration(i)*time.Minute).Format("2006-01-02 15:04:05"), job.triggeredBy, 1000, 0, job.message)
		assert.NoError(t, err, "Should insert job run")
	}

	// Test loading runs for job_a only
	jrs, err := LoadJobRuns(db, "job_a", 10, true)
	assert.NoError(t, err, "Should load job_a runs successfully")
	assert.Len(t, jrs, 2, "Should return 2 runs for job_a")
	assert.Equal(t, "webhook", jrs[0].TriggeredBy, "Latest job_a run should be webhook trigger")
	assert.Equal(t, "manual", jrs[1].TriggeredBy, "Earlier job_a run should be manual trigger")

	// Test loading runs for job_b only
	jrs, err = LoadJobRuns(db, "job_b", 10, true)
	assert.NoError(t, err, "Should load job_b runs successfully")
	assert.Len(t, jrs, 2, "Should return 2 runs for job_b")
	assert.Equal(t, "manual", jrs[0].TriggeredBy, "Latest job_b run should be manual trigger")
	assert.Equal(t, "cron", jrs[1].TriggeredBy, "Earlier job_b run should be cron trigger")

	// Test loading runs for job_c only
	jrs, err = LoadJobRuns(db, "job_c", 10, true)
	assert.NoError(t, err, "Should load job_c runs successfully")
	assert.Len(t, jrs, 1, "Should return 1 run for job_c")
	assert.Equal(t, "manual", jrs[0].TriggeredBy, "job_c run should be manual trigger")
}
