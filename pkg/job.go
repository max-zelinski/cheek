package cheek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/adhocore/gronx"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// Global status constants
const (
	StatusOK    int = 0
	StatusError int = -1
)

// OnEvent contains specs on what needs to happen after a job event.
type OnEvent struct {
	TriggerJob           []string `yaml:"trigger_job,omitempty" json:"trigger_job,omitempty"`
	NotifyWebhook        []string `yaml:"notify_webhook,omitempty" json:"notify_webhook,omitempty"`
	NotifySlackWebhook   []string `yaml:"notify_slack_webhook,omitempty" json:"notify_slack_webhook,omitempty"`
	NotifyDiscordWebhook []string `yaml:"notify_discord_webhook,omitempty" json:"notify_discord_webhook,omitempty"`
}

// JobSpec holds specifications and metadata of a job.
type JobSpec struct {
	Yaml string `yaml:"-" json:"yaml,omitempty"`

	Cron    string      `yaml:"cron,omitempty" json:"cron,omitempty"`
	Command stringArray `yaml:"command" json:"command"`

	OnSuccess OnEvent `yaml:"on_success,omitempty" json:"on_success,omitempty"`
	OnError   OnEvent `yaml:"on_error,omitempty" json:"on_error,omitempty"`

	Name                       string            `json:"name"`
	Retries                    int               `yaml:"retries,omitempty" json:"retries,omitempty"`
	Env                        map[string]secret `yaml:"env,omitempty"`
	WorkingDirectory           string            `yaml:"working_directory,omitempty" json:"working_directory,omitempty"`
	DisableConcurrentExecution bool              `yaml:"disable_concurrent_execution,omitempty" json:"disable_concurrent_execution,omitempty"`
	globalSchedule             *Schedule
	Runs                       []JobRun `json:"runs" yaml:"-"`

	nextTick time.Time
	log      zerolog.Logger
	cfg      Config
	mutex    sync.Mutex
}

type secret string

// custom marshaller to hide secrets
func (secret) MarshalText() ([]byte, error) {
	return []byte("***"), nil
}

// JobRun holds information about a job execution.
type JobRun struct {
	LogEntryId  int  `json:"id,omitempty" db:"id"`
	Status      *int `json:"status,omitempty" db:"status,omitempty"`
	logBuf      bytes.Buffer
	Log         string        `json:"log" db:"message"`
	Name        string        `json:"name" db:"job"`
	TriggeredAt time.Time     `json:"triggered_at" db:"triggered_at"`
	TriggeredBy string        `json:"triggered_by" db:"triggered_by,omitempty"`
	Triggered   []string      `json:"triggered,omitempty"`
	Duration    time.Duration `json:"duration,omitempty" db:"duration"`
	jobRef      *JobSpec
}

func (jr *JobRun) flushLogBuffer() {
	jr.Log = jr.logBuf.String()
}

func (j *JobSpec) setup(trigger string) JobRun {
	// Initialize the JobRun before executing the command
	jr := JobRun{
		Name:        j.Name,
		TriggeredAt: j.now(),
		TriggeredBy: trigger,
		Status:      nil,
		jobRef:      j,
	}

	// Log the job run immediately to the database to mark the job as started
	jr.logToDb()

	return jr
}

func (jr *JobRun) logToDb() {
	if jr.jobRef.cfg.DB == nil {
		jr.jobRef.log.Warn().Str("job", jr.Name).Msg("No db connection, not saving job log to db.")
		return
	}

	err := InsertOrUpdateJobRun(jr.jobRef.cfg.DB, jr)
	if err != nil {
		if jr.jobRef.globalSchedule != nil {
			jr.jobRef.globalSchedule.log.Warn().Str("job", jr.Name).Err(err).Msg("Couldn't save job log to db.")
		} else {
			panic(err)
		}
	}
}

func (j *JobSpec) finalize(jr *JobRun) {
	// flush logbuf to string
	jr.flushLogBuffer()
	// write logs to disk
	jr.logToDb()
	// if no DB, store run in memory for testing/debugging
	if j.cfg.DB == nil {
		j.Runs = append(j.Runs, *jr)
	}
	// launch on_events
	j.OnEvent(jr)
}

func (j *JobSpec) execCommandWithRetry(trigger string) JobRun {
	return j.execCommandWithRetryContext(context.Background(), trigger)
}

func (j *JobSpec) execCommandWithRetryContext(ctx context.Context, trigger string) JobRun {
	tries := 0
	var jr JobRun
	const timeOut = 5 * time.Second

	// Initialize the JobRun with the first trigger
	jr = j.setup(trigger)

	for tries < j.Retries+1 {
		// Check if context is cancelled before starting
		if ctx.Err() != nil {
			jr.Log = "Job cancelled due to scheduler shutdown"
			exitCode := StatusError
			jr.Status = &exitCode
			j.finalize(&jr)
			return jr
		}

		switch tries {
		case 0:
			// First attempt with the original trigger
			jr = j.execCommandContext(ctx, jr, trigger)
		default:
			// On retries, update the trigger with retry count and rerun
			jr = j.execCommandContext(ctx, jr, fmt.Sprintf("%s[retry=%d]", trigger, tries))
		}

		// Finalize logging, etc.
		j.finalize(&jr)

		if *jr.Status == StatusOK {
			// Exit if the job succeeded (Status 0)
			break
		}

		// Log the unsuccessful attempt and retry
		j.log.Debug().Str("job", j.Name).Int("exitcode", *jr.Status).Msgf("job exited unsuccessfully, launching retry after %v timeout.", timeOut)

		// Increment the attempt counter
		tries++

		// Sleep with context cancellation check
		select {
		case <-time.After(timeOut):
			// Continue to retry
		case <-ctx.Done():
			jr.Log += "\nJob cancelled during retry timeout"
			exitCode := StatusError
			jr.Status = &exitCode
			return jr
		}
	}

	return jr
}

func (j *JobSpec) now() time.Time {
	// defer for if schedule doesn't exist, allows for easy testing
	if j.globalSchedule != nil {
		return j.globalSchedule.now()
	}
	return time.Now()
}

func (j *JobSpec) execCommand(jr JobRun, trigger string) JobRun {
	return j.execCommandContext(context.Background(), jr, trigger)
}

func (j *JobSpec) execCommandContext(ctx context.Context, jr JobRun, trigger string) JobRun {
	j.log.Info().Str("job", j.Name).Str("trigger", trigger).Msgf("Job triggered")
	suppressLogs := j.cfg.SuppressLogs

	var cmd *exec.Cmd
	switch len(j.Command) {
	case 0:
		err := errors.New("no command specified")
		jr.Log = fmt.Sprintf("Job unable to start: %v", err.Error())
		j.log.Warn().Str("job", j.Name).Str("trigger", trigger).Err(err).Msg(jr.Log)
		if !suppressLogs {
			fmt.Println(err.Error())
		}
		errStatus := StatusError
		jr.Status = &errStatus // Set failure status when no command is specified

		return jr
	case 1:
		cmd = exec.CommandContext(ctx, j.Command[0])
	default:
		cmd = exec.CommandContext(ctx, j.Command[0], j.Command[1:]...)
	}

	// Add env vars
	cmd.Env = os.Environ()
	for k, v := range j.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Dir = j.WorkingDirectory

	var w io.Writer
	switch j.cfg.SuppressLogs {
	case true:
		w = &jr.logBuf
	default:
		w = io.MultiWriter(os.Stdout, &jr.logBuf)
	}

	// Merge stdout and stderr to same writer
	cmd.Stdout = w
	cmd.Stderr = w

	// Start command execution
	err := cmd.Start()
	if err != nil {
		// Existing logging logic
		if !suppressLogs {
			fmt.Println(err.Error())
		}

		// Log the initial error and set the exit code
		exitCode := StatusError
		j.log.Warn().Str("job", j.Name).Str("trigger", trigger).Int("exitcode", exitCode).Err(err).Msg("job unable to start")

		// Also send this to terminal output
		logMessage := fmt.Sprintf("job unable to start: %v", err.Error())
		_, writeErr := w.Write([]byte(logMessage)) // Ensure we log this message
		if writeErr != nil {
			j.log.Debug().Str("job", j.Name).Err(writeErr).Msg("can't write to log buffer")
		}
		jr.Log = logMessage   // Capture log message to jr.Log
		jr.Status = &exitCode // Set the exit code in the job result
		return jr
	}

	// Wait for the command to finish and check for errors
	if err := cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			// Check if it was killed due to context cancellation
			if ctx.Err() != nil {
				jr.Log += "\nJob killed due to scheduler shutdown"
				exitCode := StatusError
				jr.Status = &exitCode
				j.log.Info().Str("job", j.Name).Msg("Job killed due to context cancellation")
			} else {
				// Get the exact exit code from ExitError
				exitCode := exitError.ExitCode()
				jr.Status = &exitCode // Set the exit code in the job result
				j.log.Warn().Str("job", j.Name).Msgf("Exit code: %d", exitCode)
				jr.Log += fmt.Sprintf("Exit code: %d\n", exitCode)
			}
		} else {
			// Handle unexpected errors
			exitCode := StatusError
			j.log.Error().Str("job", j.Name).Err(err).Msg("unexpected error during command execution")
			jr.Status = &exitCode
			return jr
		}
	} else {
		// No error, command exited successfully
		StatusCode := StatusOK
		jr.Status = &StatusCode // Command succeeded, set exit code 0
	}

	jr.Duration = time.Duration(time.Since(jr.TriggeredAt).Milliseconds())

	j.log.Debug().Str("job", j.Name).Int("exitcode", *jr.Status).Msgf("job exited with status: %d", *jr.Status)

	return jr
}

func (j *JobSpec) loadLogFromDb(id int) (JobRun, error) {
	var jr JobRun
	if j.cfg.DB == nil {
		j.log.Warn().Str("job", j.Name).Msg("No db connection, not loading job run from db.")
		return jr, errors.New("no db connection")
	}

	jr, err := LoadJobRun(j.cfg.DB, j.Name, id)
	if err != nil {
		j.log.Warn().Str("job", j.Name).Err(err).Msg("Couldn't load job run from db.")
		return jr, err
	}
	return jr, nil
}

func (j *JobSpec) loadRunsFromDb(nruns int, includeLogs bool) {
	if j.cfg.DB == nil {
		j.log.Warn().Str("job", j.Name).Msg("No db connection, not loading job runs from db.")
		return
	}

	jrs, err := LoadJobRuns(j.cfg.DB, j.Name, nruns, includeLogs)
	if err != nil {
		j.log.Warn().Str("job", j.Name).Err(err).Msg("Couldn't load job runs from db.")
		return
	}
	j.Runs = jrs
}

func (j *JobSpec) setNextTick(refTime time.Time, includeRefTime bool) error {
	if j.Cron != "" {
		t, err := gronx.NextTickAfter(j.Cron, refTime, includeRefTime)
		j.nextTick = t
		return err
	}
	return nil
}

func (j *JobSpec) ValidateCron() error {
	if j.Cron != "" {
		gronx := gronx.New()
		if !gronx.IsValid(j.Cron) {
			return fmt.Errorf("cron string for job '%s' not valid", j.Name)
		}
	}
	return nil
}

func (j *JobSpec) OnEvent(jr *JobRun) {
	var jobsToTrigger []string
	var webhooksToCall []webhook
	var events []OnEvent

	switch *jr.Status == StatusOK {
	case true: // after success
		events = append(events, j.OnSuccess)
		if j.globalSchedule != nil {
			events = append(events, j.globalSchedule.OnSuccess)
		}
	case false: // after error
		events = append(events, j.OnError)
		if j.globalSchedule != nil {
			events = append(events, j.globalSchedule.OnError)
		}
	}

	for _, e := range events {
		jobsToTrigger = append(jobsToTrigger, e.TriggerJob...)
		for _, whURL := range e.NotifyWebhook {
			webhooksToCall = append(webhooksToCall, NewDefaultWebhook(whURL))
		}
		for _, whURL := range e.NotifySlackWebhook {
			webhooksToCall = append(webhooksToCall, NewSlackWebhook(whURL))
		}
		for _, whURL := range e.NotifyDiscordWebhook {
			webhooksToCall = append(webhooksToCall, NewDiscordWebhook(whURL))
		}
	}

	var wg sync.WaitGroup

	for _, tn := range jobsToTrigger {
		tj := j.globalSchedule.Jobs[tn]
		j.log.Debug().Str("job", j.Name).Str("on_event", "job_trigger").Msg("triggered by parent job")
		wg.Add(1)
		go func(wg *sync.WaitGroup, tj *JobSpec) {
			defer wg.Done()
			if tj.DisableConcurrentExecution {
				tj.mutex.Lock()
				defer tj.mutex.Unlock()
			}
			// Use background context for triggered jobs (they should complete independently)
			tj.execCommandWithRetry(fmt.Sprintf("job[%s]", j.Name))
		}(&wg, tj)
	}

	// trigger webhooks
	for _, wu := range webhooksToCall {
		j.log.Debug().Str("job", j.Name).Str("on_event", wu.Name()+"_webhook_call").Msg("triggered by parent job")
		wg.Add(1)
		go func(wg *sync.WaitGroup, wu webhook) {
			defer wg.Done()
			resp_body, err := wu.Call(jr)
			if err != nil {
				j.log.Warn().Str("job", j.Name).Str("on_event", "webhook").Err(err).Msg("webhook notify failed")
			}
			j.log.Debug().Str("job", jr.Name).Str("webhook_call", "response").Str("webhook_url", wu.URL()).Msg(string(resp_body))
		}(&wg, wu)
	}

	wg.Wait() // this allows to wait for go routines when running just the job exec
}

func (j *JobSpec) ToYAML(includeRuns bool) (string, error) {
	if !includeRuns {
		j.Runs = []JobRun{}
	}

	yData, err := yaml.Marshal(j)
	if err != nil {
		return "", err
	}
	return string(yData), nil
}

// RunJob allows to run a specific job
func RunJob(log zerolog.Logger, cfg Config, scheduleFn string, jobName string) (JobRun, error) {
	s, err := loadSchedule(log, cfg, scheduleFn)
	if err != nil {
		log.Error().Err(err).Msgf("error loading schedule: %s", scheduleFn)
		return JobRun{}, fmt.Errorf("failed to load schedule: %w", err)
	}

	for _, job := range s.Jobs {
		if job.Name == jobName {
			// Use the setup function to create a JobRun instance
			jr := job.setup("manual")

			// Execute the command with the initialized JobRun and the trigger string
			jr = job.execCommand(jr, "manual")
			job.finalize(&jr)
			return jr, nil
		}
	}

	return JobRun{}, fmt.Errorf("cannot find job %s in schedule %s", jobName, scheduleFn)
}
