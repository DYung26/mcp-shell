package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

const (
	jobQueued       = "queued"
	jobRunning      = "running"
	jobSucceeded    = "succeeded"
	jobFailed       = "failed"
	jobCancelled    = "cancelled"
	jobTimedOut     = "timed_out"
	maxActiveJobs   = 32
	maxRetainedJobs = 1000
)

type JobRecord struct {
	ID                string        `json:"job_id"`
	Command           string        `json:"command"`
	Status            string        `json:"status"`
	PID               int           `json:"pid,omitempty"`
	ChildPID          int           `json:"child_pid,omitempty"`
	ExitCode          *int          `json:"exit_code,omitempty"`
	Error             string        `json:"error,omitempty"`
	OutputTruncated   bool          `json:"output_truncated"`
	CreatedAt         time.Time     `json:"created_at"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	FinishedAt        *time.Time    `json:"finished_at,omitempty"`
	ExecutionDuration time.Duration `json:"execution_duration,omitempty"`
	ExecutionTimeout  time.Duration `json:"execution_timeout,omitempty"`
}

type JobOutput struct {
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	StdoutOffset int64  `json:"stdout_offset"`
	StderrOffset int64  `json:"stderr_offset"`
}

type jobRetentionPolicy uint8

const (
	jobRetentionStrict jobRetentionPolicy = iota
	jobRetentionAllowSingleOverflow
)

type jobListOptions struct {
	Limit           int
	Offset          int64
	Status          string
	CommandContains string
}

type jobListResult struct {
	Jobs    []*JobRecord `json:"jobs"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
	Offset  int64        `json:"offset"`
	HasMore bool         `json:"has_more"`
}

type JobManager struct {
	root   string
	config SecurityConfig
	logger zerolog.Logger
	mu     sync.Mutex
}

func newJobManager(config SecurityConfig, logger zerolog.Logger) *JobManager {
	root := os.Getenv("MCP_SHELL_JOB_ROOT")
	if root == "" {
		root = os.Getenv("MCP_SHELL_JOB_DIR")
	}
	if root == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			cacheDir = os.TempDir()
		}
		root = filepath.Join(cacheDir, "mcp-shell", "jobs")
	}
	return &JobManager{root: root, config: config, logger: logger.With().Str("component", "jobs").Logger()}
}

func (m *JobManager) start(command string, executionTimeout time.Duration) (*JobRecord, error) {
	return m.startWithRetentionPolicy(command, executionTimeout, jobRetentionStrict)
}

func (m *JobManager) startShellExec(command string, executionTimeout time.Duration) (*JobRecord, error) {
	return m.startWithRetentionPolicy(command, executionTimeout, jobRetentionAllowSingleOverflow)
}

func (m *JobManager) startWithRetentionPolicy(command string, executionTimeout time.Duration, retentionPolicy jobRetentionPolicy) (*JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return nil, err
	}
	records, err := m.list()
	if err != nil {
		return nil, err
	}
	active := 0
	for _, job := range records {
		if job.Status == jobQueued || job.Status == jobRunning {
			active++
		}
	}
	if active >= maxActiveJobs {
		return nil, fmt.Errorf("background job limit reached (%d active)", maxActiveJobs)
	}
	if len(records) >= maxRetainedJobs && (retentionPolicy != jobRetentionAllowSingleOverflow || len(records) > maxRetainedJobs) {
		return nil, fmt.Errorf("job retention limit reached; remove old records from %s", m.root)
	}

	id := uuid.NewString()
	job := &JobRecord{ID: id, Command: command, Status: jobQueued, CreatedAt: time.Now().UTC(), ExecutionTimeout: executionTimeout}
	if err := m.write(job); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "MCP_SHELL_JOB_ROOT="+m.root, "MCP_SHELL_JOB_COMMAND="+base64.StdEncoding.EncodeToString([]byte(command)), "MCP_SHELL_JOB_WORKER=1", "MCP_SHELL_JOB_ID="+id)
	configureDetachedWorker(cmd)
	if err := cmd.Start(); err != nil {
		job.Status, job.Error = jobFailed, err.Error()
		finished := time.Now().UTC()
		job.FinishedAt = &finished
		_ = m.write(job)
		return nil, fmt.Errorf("start worker: %w", err)
	}
	pidPath := filepath.Join(m.dir(id), "pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		_ = terminateWorker(cmd.Process.Pid)
		return nil, fmt.Errorf("persist worker PID: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		m.logger.Warn().Err(err).Str("job_id", id).Msg("Could not release worker handle")
	}
	return job, nil
}

func (m *JobManager) get(id string, stdoutOffset, stderrOffset int64, useBase64 bool) (*JobRecord, *JobOutput, error) {
	if err := validateJobID(id); err != nil {
		return nil, nil, err
	}
	job, err := m.read(id)
	if err != nil {
		return nil, nil, err
	}
	job, err = m.reconcile(job)
	if err != nil {
		return nil, nil, err
	}
	stdout, nextStdout, err := readJobOutput(filepath.Join(m.dir(id), "stdout"), stdoutOffset)
	if err != nil {
		return nil, nil, err
	}
	stderr, nextStderr, err := readJobOutput(filepath.Join(m.dir(id), "stderr"), stderrOffset)
	if err != nil {
		return nil, nil, err
	}
	if useBase64 {
		stdout = base64.StdEncoding.EncodeToString([]byte(stdout))
		stderr = base64.StdEncoding.EncodeToString([]byte(stderr))
	}
	return job, &JobOutput{Stdout: stdout, Stderr: stderr, StdoutOffset: nextStdout, StderrOffset: nextStderr}, nil
}

func (m *JobManager) getComplete(id string, useBase64 bool) (*JobRecord, *JobOutput, error) {
	if err := validateJobID(id); err != nil {
		return nil, nil, err
	}
	job, err := m.read(id)
	if err != nil {
		return nil, nil, err
	}
	job, err = m.reconcile(job)
	if err != nil {
		return nil, nil, err
	}
	stdout, stdoutOffset, err := readCompleteJobOutput(filepath.Join(m.dir(id), "stdout"))
	if err != nil {
		return nil, nil, err
	}
	stderr, stderrOffset, err := readCompleteJobOutput(filepath.Join(m.dir(id), "stderr"))
	if err != nil {
		return nil, nil, err
	}
	if useBase64 {
		stdout = base64.StdEncoding.EncodeToString([]byte(stdout))
		stderr = base64.StdEncoding.EncodeToString([]byte(stderr))
	}
	return job, &JobOutput{
		Stdout:       stdout,
		Stderr:       stderr,
		StdoutOffset: stdoutOffset,
		StderrOffset: stderrOffset,
	}, nil
}

func (m *JobManager) list() ([]*JobRecord, error) {
	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return []*JobRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	jobs := make([]*JobRecord, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || validateJobID(entry.Name()) != nil {
			continue
		}
		job, err := m.read(entry.Name())
		if err == nil {
			job, err = m.reconcile(job)
		}
		if err != nil {
			m.logger.Warn().Err(err).Str("job_id", entry.Name()).Msg("Skipping unreadable job")
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return jobs, nil
}

func (m *JobManager) listPage(options jobListOptions) (*jobListResult, error) {
	jobs, err := m.list()
	if err != nil {
		return nil, err
	}

	filtered := make([]*JobRecord, 0, len(jobs))
	commandFilter := strings.ToLower(options.CommandContains)
	for _, job := range jobs {
		if options.Status != "" && job.Status != options.Status {
			continue
		}
		if commandFilter != "" && !strings.Contains(strings.ToLower(job.Command), commandFilter) {
			continue
		}
		filtered = append(filtered, job)
	}

	total := len(filtered)
	start := options.Offset
	if start > int64(total) {
		start = int64(total)
	}
	end := start + int64(options.Limit)
	if end > int64(total) {
		end = int64(total)
	}

	return &jobListResult{
		Jobs:    filtered[int(start):int(end)],
		Total:   total,
		Limit:   options.Limit,
		Offset:  options.Offset,
		HasMore: end < int64(total),
	}, nil
}

func (m *JobManager) cancel(id string) (*JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateJobID(id); err != nil {
		return nil, err
	}
	job, err := m.read(id)
	if err != nil {
		return nil, err
	}
	if job.Status != jobQueued && job.Status != jobRunning {
		return job, nil
	}
	cancelRequestedAt := time.Now().UTC()
	pid := job.PID
	if pid <= 0 {
		data, readErr := os.ReadFile(filepath.Join(m.dir(id), "pid"))
		if readErr != nil {
			return nil, fmt.Errorf("job %s has no worker PID: %w", id, readErr)
		}
		pid, err = strconv.Atoi(string(data))
		if err != nil {
			return nil, fmt.Errorf("invalid worker PID for job %s: %w", id, err)
		}
	}
	childPID := job.ChildPID
	if childPID <= 0 {
		for attempt := 0; attempt < 10; attempt++ {
			if data, readErr := os.ReadFile(filepath.Join(m.dir(id), "child_pid")); readErr == nil {
				childPID, _ = strconv.Atoi(string(data))
				if childPID > 0 {
					break
				}
			}
			if !workerIsAlive(pid) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childPID > 0 {
		if err := terminateCommandProcess(childPID); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return nil, fmt.Errorf("terminate job command process: %w", err)
		}
	}
	if err := terminateWorker(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return nil, err
	}
	if current, readErr := m.read(id); readErr == nil &&
		current.Status != jobQueued && current.Status != jobRunning &&
		current.FinishedAt != nil && current.FinishedAt.Before(cancelRequestedAt) {
		return current, nil
	}
	job.Status, job.Error = jobCancelled, "cancelled by request"
	finished := time.Now().UTC()
	job.FinishedAt = &finished
	if err := m.write(job); err != nil {
		return nil, err
	}
	return job, nil
}

func (m *JobManager) wait(ctx context.Context, id string, duration time.Duration) (*JobRecord, error) {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := m.read(id)
		if err != nil {
			return nil, err
		}
		job, err = m.reconcile(job)
		if err != nil {
			return nil, err
		}
		if job.Status != jobQueued && job.Status != jobRunning {
			return job, nil
		}
		select {
		case <-deadline.C:
			return job, nil
		case <-ctx.Done():
			return job, nil
		case <-ticker.C:
		}
	}
}

func (m *JobManager) delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateJobID(id); err != nil {
		return err
	}
	job, err := m.read(id)
	if err != nil {
		return err
	}
	job, err = m.reconcile(job)
	if err != nil {
		return err
	}
	if job.Status == jobQueued || job.Status == jobRunning {
		return fmt.Errorf("cannot delete active job %s; cancel it first", id)
	}
	return os.RemoveAll(m.dir(id))
}

func (m *JobManager) runWorker(id string) error {
	if err := validateJobID(id); err != nil {
		return err
	}
	command, err := base64.StdEncoding.DecodeString(os.Getenv("MCP_SHELL_JOB_COMMAND"))
	if err != nil {
		return err
	}
	for _, key := range []string{"MCP_SHELL_JOB_ROOT", "MCP_SHELL_JOB_COMMAND", "MCP_SHELL_JOB_WORKER", "MCP_SHELL_JOB_ID"} {
		_ = os.Unsetenv(key)
	}
	job, err := m.read(id)
	if err != nil {
		return err
	}
	validator := newSecurityValidator(m.config, m.logger)
	if err := validator.validateCommand(string(command)); err != nil {
		job.Status, job.Error = jobFailed, err.Error()
		finished := time.Now().UTC()
		job.FinishedAt = &finished
		return m.write(job)
	}
	started := time.Now().UTC()
	job.Status, job.StartedAt, job.PID = jobRunning, &started, os.Getpid()
	if err := m.write(job); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir(id), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(m.dir(id), "stdout"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	errout, err := os.OpenFile(filepath.Join(m.dir(id), "stderr"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer errout.Close()

	timeout := job.ExecutionTimeout
	if timeout <= 0 {
		timeout = m.config.MaxBackgroundExecutionTime
	}
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	executor := newCommandExecutor(m.config, m.logger)
	result, executeErr := executor.executeSecureCommandTo(ctx, string(command), out, errout, func(pid int) error {
		job.ChildPID = pid
		if err := os.WriteFile(filepath.Join(m.dir(id), "child_pid"), []byte(strconv.Itoa(pid)), 0600); err != nil {
			return fmt.Errorf("persist background command PID: %w", err)
		}
		if err := m.write(job); err != nil {
			return fmt.Errorf("persist background command state: %w", err)
		}
		return nil
	})
	finished := time.Now().UTC()
	job.FinishedAt = &finished
	job.ExecutionDuration = finished.Sub(started)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		job.Status, job.Error = jobTimedOut, fmt.Sprintf("execution exceeded %s", timeout)
	case executeErr != nil:
		job.Status, job.Error = jobFailed, executeErr.Error()
	default:
		job.ExitCode = &result.ExitCode
		job.OutputTruncated = result.OutputTruncated
		if result.Status == "success" {
			job.Status = jobSucceeded
		} else {
			job.Status = jobFailed
		}
	}
	return m.write(job)
}

func (m *JobManager) write(job *JobRecord) error {
	if err := os.MkdirAll(m.dir(job.ID), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(m.dir(job.ID), "job.json")
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *JobManager) read(id string) (*JobRecord, error) {
	data, err := os.ReadFile(filepath.Join(m.dir(id), "job.json"))
	if err != nil {
		return nil, fmt.Errorf("read job %s: %w", id, err)
	}
	var job JobRecord
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (m *JobManager) reconcile(job *JobRecord) (*JobRecord, error) {
	if job.Status != jobQueued && job.Status != jobRunning {
		return job, nil
	}
	pid := job.PID
	if pid <= 0 {
		data, err := os.ReadFile(filepath.Join(m.dir(job.ID), "pid"))
		if err != nil {
			return job, nil
		}
		pid, err = strconv.Atoi(string(data))
		if err != nil {
			return nil, err
		}
	}
	if workerIsAlive(pid) {
		return job, nil
	}
	childPID := job.ChildPID
	if childPID <= 0 {
		if data, err := os.ReadFile(filepath.Join(m.dir(job.ID), "child_pid")); err == nil {
			childPID, _ = strconv.Atoi(string(data))
		}
	}
	if childPID > 0 {
		_ = terminateCommandProcess(childPID)
	}
	job.Status = jobFailed
	job.Error = "background worker exited before persisting a final result"
	finished := time.Now().UTC()
	job.FinishedAt = &finished
	if err := m.write(job); err != nil {
		return nil, err
	}
	return job, nil
}

func (m *JobManager) dir(id string) string { return filepath.Join(m.root, id) }

func readJobOutput(path string, offset int64) (string, int64, error) {
	if offset < 0 {
		return "", 0, errors.New("output offset cannot be negative")
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if offset > info.Size() {
		return "", 0, fmt.Errorf("offset %d exceeds output size %d", offset, info.Size())
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	data := make([]byte, 256*1024)
	n, err := file.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	return string(data[:n]), offset + int64(n), nil
}

func readCompleteJobOutput(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return string(data), int64(len(data)), nil
}

func validateJobID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid job_id %q", id)
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("invalid job_id %q", id)
	}
	return nil
}
