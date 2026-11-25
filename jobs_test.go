//go:build !windows

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestJobManager_startRunsDurableWorkerAndRetrievesResult(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "security.yaml")
	configData := []byte("security:\n  enabled: true\n  use_shell_execution: false\n  allowed_executables: [echo, sleep]\n  max_background_execution_time: 5s\n  max_output_size: 1024\n  working_directory: /tmp\n")
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	t.Setenv("MCP_SHELL_JOB_DIR", filepath.Join(root, "jobs"))
	t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)
	logger := zerolog.New(zerolog.NewTestWriter(t))
	manager := newJobManager(SecurityConfig{Enabled: true, AllowedExecutables: []string{"echo", "sleep"}, MaxBackgroundExecutionTime: 5 * time.Second, MaxOutputSize: 1024}, logger)

	job, err := manager.start("echo background-ok", 0)
	require.NoError(t, err)
	job, err = manager.wait(context.Background(), job.ID, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobSucceeded, job.Status)
	_, output, err := manager.get(job.ID, 0, 0, false)
	require.NoError(t, err)
	require.Contains(t, output.Stdout, "background-ok")
	pidBytes, err := os.ReadFile(filepath.Join(manager.dir(job.ID), "pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}, time.Second, 10*time.Millisecond)
}

func TestJobManager_cancelTerminatesRunningWorker(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "security.yaml")
	configData := []byte("security:\n  enabled: true\n  use_shell_execution: false\n  allowed_executables: [echo, sleep]\n  max_background_execution_time: 5s\n  max_output_size: 1024\n  working_directory: /tmp\n")
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	t.Setenv("MCP_SHELL_JOB_DIR", filepath.Join(root, "jobs"))
	t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)
	logger := zerolog.New(zerolog.NewTestWriter(t))
	manager := newJobManager(SecurityConfig{Enabled: true, AllowedExecutables: []string{"echo", "sleep"}, MaxBackgroundExecutionTime: 5 * time.Second, MaxOutputSize: 1024}, logger)

	job, err := manager.start("sleep 30", 0)
	require.NoError(t, err)
	job, err = manager.cancel(job.ID)
	require.NoError(t, err)
	require.Equal(t, jobCancelled, job.Status)
}

func TestJobManager_runWorkerPersistsOutputAndStatus(t *testing.T) {
	root := t.TempDir()
	logger := zerolog.New(zerolog.NewTestWriter(t))
	config := SecurityConfig{
		Enabled:                    true,
		AllowedExecutables:         []string{"echo"},
		MaxBackgroundExecutionTime: time.Second * 5,
		MaxOutputSize:              1024,
	}
	manager := &JobManager{root: root, config: config, logger: logger}
	job := &JobRecord{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Command: "echo hello", Status: jobQueued, CreatedAt: time.Now().UTC()}
	require.NoError(t, manager.write(job))
	t.Setenv("MCP_SHELL_JOB_COMMAND", base64.StdEncoding.EncodeToString([]byte(job.Command)))

	require.NoError(t, manager.runWorker(job.ID))

	record, output, err := manager.get(job.ID, 0, 0, false)
	require.NoError(t, err)
	require.Equal(t, jobSucceeded, record.Status)
	require.NotNil(t, record.ExitCode)
	require.Zero(t, *record.ExitCode)
	require.Contains(t, output.Stdout, "hello")
	require.Equal(t, int64(6), output.StdoutOffset)
	require.Empty(t, output.Stderr)
	require.NotNil(t, record.StartedAt)
	require.NotNil(t, record.FinishedAt)
}

func TestJobManager_runWorkerUsesJobExecutionTimeout(t *testing.T) {
	manager := &JobManager{
		root: t.TempDir(),
		config: SecurityConfig{
			Enabled:                    true,
			AllowedExecutables:         []string{"sleep"},
			MaxBackgroundExecutionTime: 5 * time.Second,
		},
		logger: zerolog.New(zerolog.NewTestWriter(t)),
	}
	job := &JobRecord{
		ID:               "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0",
		Command:          "sleep 1",
		Status:           jobQueued,
		CreatedAt:        time.Now().UTC(),
		ExecutionTimeout: 100 * time.Millisecond,
	}
	require.NoError(t, manager.write(job))
	t.Setenv("MCP_SHELL_JOB_COMMAND", base64.StdEncoding.EncodeToString([]byte(job.Command)))

	require.NoError(t, manager.runWorker(job.ID))

	record, _, err := manager.get(job.ID, 0, 0, false)
	require.NoError(t, err)
	require.Equal(t, jobTimedOut, record.Status)
	require.Equal(t, 100*time.Millisecond, record.ExecutionTimeout)
}

func TestJobManager_getReturnsIncrementalOutput(t *testing.T) {
	manager := &JobManager{root: t.TempDir()}
	job := &JobRecord{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Status: jobRunning, CreatedAt: time.Now().UTC()}
	require.NoError(t, manager.write(job))
	require.NoError(t, os.WriteFile(filepath.Join(manager.dir(job.ID), "stdout"), []byte("first-second-third"), 0600))

	_, first, err := manager.get(job.ID, 0, 0, false)
	require.NoError(t, err)
	require.Equal(t, "first-second-third", first.Stdout)
	_, second, err := manager.get(job.ID, 6, 0, false)
	require.NoError(t, err)
	require.Equal(t, "second-third", second.Stdout)
	require.Equal(t, int64(18), second.StdoutOffset)
}

func TestJobManager_getCompleteReturnsAllOutput(t *testing.T) {
	manager := &JobManager{root: t.TempDir()}
	job := &JobRecord{
		ID:        "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0",
		Status:    jobSucceeded,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, manager.write(job))

	stdout := strings.Repeat("a", 600*1024)
	stderr := strings.Repeat("b", 300*1024)
	require.NoError(t, os.WriteFile(filepath.Join(manager.dir(job.ID), "stdout"), []byte(stdout), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(manager.dir(job.ID), "stderr"), []byte(stderr), 0600))

	_, output, err := manager.getComplete(job.ID, false)
	require.NoError(t, err)
	require.Equal(t, stdout, output.Stdout)
	require.Equal(t, stderr, output.Stderr)
	require.Equal(t, int64(len(stdout)), output.StdoutOffset)
	require.Equal(t, int64(len(stderr)), output.StderrOffset)
}

func TestJobManager_listPageFiltersAndPaginates(t *testing.T) {
	manager := &JobManager{root: t.TempDir()}
	createdAt := time.Now().UTC()
	jobs := []*JobRecord{
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Command: "go test ./...", Status: jobSucceeded, CreatedAt: createdAt},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f1", Command: "git status", Status: jobFailed, CreatedAt: createdAt.Add(-time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f2", Command: "go test ./pkg", Status: jobSucceeded, CreatedAt: createdAt.Add(-2 * time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f3", Command: "go build", Status: jobSucceeded, CreatedAt: createdAt.Add(-3 * time.Minute)},
	}
	for _, job := range jobs {
		require.NoError(t, manager.write(job))
	}

	page, err := manager.listPage(jobListOptions{
		Limit:           1,
		Offset:          1,
		Status:          jobSucceeded,
		CommandContains: "GO TEST",
	})
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, 1, page.Limit)
	require.Equal(t, int64(1), page.Offset)
	require.False(t, page.HasMore)
	require.Len(t, page.Jobs, 1)
	require.Equal(t, jobs[2].ID, page.Jobs[0].ID)

	emptyPage, err := manager.listPage(jobListOptions{Limit: 25, Offset: 10})
	require.NoError(t, err)
	require.Empty(t, emptyPage.Jobs)
	require.Equal(t, 4, emptyPage.Total)
}

func TestJobManager_rejectsInvalidJobIDsAndOffsets(t *testing.T) {
	manager := &JobManager{root: t.TempDir()}
	_, _, err := manager.get("../outside", 0, 0, false)
	require.Error(t, err)

	_, _, err = manager.get("f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", -1, 0, false)
	require.Error(t, err)
}

func TestLimitedWriterCapsOutputAndReportsTruncation(t *testing.T) {
	var output strings.Builder
	writer := newLimitedWriter(&output, 5)
	_, err := writer.Write([]byte("123"))
	require.NoError(t, err)
	_, err = writer.Write([]byte("4567"))
	require.NoError(t, err)
	require.Equal(t, "12345", output.String())
	require.True(t, writer.truncated)
}

func TestJobManager_pruneCompletedJobsRemovesOldestTerminalRecordsOnly(t *testing.T) {
	root := t.TempDir()
	manager := &JobManager{
		root:   root,
		config: SecurityConfig{MaxRetainedJobs: 3},
		logger: zerolog.New(zerolog.NewTestWriter(t)),
	}
	created := time.Now().UTC()
	jobs := []*JobRecord{
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Status: jobSucceeded, CreatedAt: created.Add(-4 * time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f1", Status: jobRunning, CreatedAt: created.Add(-3 * time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f2", Status: jobFailed, CreatedAt: created.Add(-2 * time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f3", Status: jobSucceeded, CreatedAt: created.Add(-time.Minute)},
	}
	for _, job := range jobs {
		require.NoError(t, manager.write(job))
	}

	// list() sorts newest first; pruning walks from the end and skips active jobs.
	records, err := manager.list()
	require.NoError(t, err)
	require.NoError(t, manager.pruneCompletedJobs(records))

	_, err = os.Stat(manager.dir(jobs[0].ID))
	require.ErrorIs(t, err, os.ErrNotExist, "oldest completed job should be pruned")
	_, err = os.Stat(manager.dir(jobs[1].ID))
	require.NoError(t, err, "active job must never be pruned")
	_, err = os.Stat(manager.dir(jobs[2].ID))
	require.ErrorIs(t, err, os.ErrNotExist, "next-oldest completed job should also be pruned to make room")
	_, err = os.Stat(manager.dir(jobs[3].ID))
	require.NoError(t, err, "newest completed job should be retained")
}

func TestJobManager_pruneCompletedJobsUsesDefaultLimit(t *testing.T) {
	manager := &JobManager{root: t.TempDir(), logger: zerolog.New(zerolog.NewTestWriter(t))}
	created := time.Now().UTC()
	jobs := []*JobRecord{
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Status: jobSucceeded, CreatedAt: created.Add(-time.Minute)},
	}
	for _, job := range jobs {
		require.NoError(t, manager.write(job))
	}
	records, err := manager.list()
	require.NoError(t, err)
	require.NoError(t, manager.pruneCompletedJobs(records))
	_, err = os.Stat(manager.dir(jobs[0].ID))
	require.NoError(t, err, "default limit should not prune below capacity")
}

func TestJobManager_deleteRejectsActiveAndRemovesCompletedJobs(t *testing.T) {
	manager := &JobManager{root: t.TempDir()}
	job := &JobRecord{
		ID:        "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0",
		Status:    jobRunning,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, manager.write(job))
	require.Error(t, manager.delete(job.ID))

	job.Status = jobSucceeded
	require.NoError(t, manager.write(job))
	require.NoError(t, manager.delete(job.ID))
	_, err := os.Stat(manager.dir(job.ID))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestJobManager_timeoutTerminatesCommandProcessGroup(t *testing.T) {
	manager := &JobManager{
		root: t.TempDir(),
		config: SecurityConfig{
			Enabled:                    true,
			AllowedExecutables:         []string{"sleep"},
			MaxBackgroundExecutionTime: 100 * time.Millisecond,
			MaxOutputSize:              1024,
		},
		logger: zerolog.New(zerolog.NewTestWriter(t)),
	}
	job := &JobRecord{
		ID:        "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0",
		Command:   "sleep 30",
		Status:    jobQueued,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, manager.write(job))
	t.Setenv("MCP_SHELL_JOB_COMMAND", base64.StdEncoding.EncodeToString([]byte(job.Command)))

	require.NoError(t, manager.runWorker(job.ID))
	record, err := manager.read(job.ID)
	require.NoError(t, err)
	require.Equal(t, jobTimedOut, record.Status)
	require.Greater(t, record.ChildPID, 0)
	require.False(t, workerIsAlive(record.ChildPID))
}
