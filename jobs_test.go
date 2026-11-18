//go:build !windows

package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
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

	job, err := manager.start("echo background-ok")
	require.NoError(t, err)
	job, err = manager.wait(context.Background(), job.ID, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobSucceeded, job.Status)
	_, output, err := manager.get(job.ID, 0, 0, false)
	require.NoError(t, err)
	require.Contains(t, output.Stdout, "background-ok")
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

	job, err := manager.start("sleep 30")
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
