//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

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
	manager := &JobManager{root: t.TempDir(), logger: zerolog.New(zerolog.NewTestWriter(t))}
	job := &JobRecord{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Status: jobRunning, CreatedAt: time.Now().UTC()}
	require.NoError(t, manager.write(job))
	require.Error(t, manager.delete(job.ID))
	job.Status = jobSucceeded
	require.NoError(t, manager.write(job))
	require.NoError(t, manager.delete(job.ID))
	_, err := os.Stat(manager.dir(job.ID))
	require.ErrorIs(t, err, os.ErrNotExist)
}
