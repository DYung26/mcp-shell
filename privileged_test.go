package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func toolRequest(args map[string]interface{}) mcp.CallToolRequest {
	r := mcp.CallToolRequest{}
	r.Params.Arguments = args
	return r
}

func TestPrivilegedRequestsAreIndependentAndOneShot(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("MCP_SHELL_PRIVILEGED_STATE_DIR", stateDir)
	t.Setenv("MCP_SHELL_SUDO_PASSWORD", "test-password")
	logger := zerolog.New(zerolog.NewTestWriter(t))
	h, err := newPrivilegedHandler(time.Second, 1024, logger)
	require.NoError(t, err)

	const count = 8
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := h.request(context.Background(), toolRequest(map[string]interface{}{
				"command": "echo request-" + string(rune('a'+i)),
				"reason":  "concurrency test",
			}))
			if err != nil || result == nil || result.IsError {
				t.Errorf("request failed: result=%v err=%v", result, err)
				return
			}
			var payload map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &payload))
			ids <- payload["request_id"].(string)
		}(i)
	}
	wg.Wait()
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		require.Len(t, id, 32)
		require.False(t, seen[id], "request IDs must be unique")
		seen[id] = true
		contents, err := os.ReadFile(filepath.Join(stateDir, id+".pending.json"))
		require.NoError(t, err)
		var record privilegedRequest
		require.NoError(t, json.Unmarshal(contents, &record))
		require.Equal(t, id, record.ID)
		require.Len(t, record.Argv, 2)
	}
	require.Len(t, seen, count)

	id := ""
	for candidate := range seen {
		id = candidate
		break
	}

	// Use a fake sudo executable to verify atomic single-use claiming without
	// requiring real root privileges in the test environment.
	binDir := t.TempDir()
	callLog := filepath.Join(t.TempDir(), "sudo-calls.log")
	fakeSudo := filepath.Join(binDir, "sudo")
	require.NoError(t, os.WriteFile(fakeSudo, []byte("#!/bin/sh\nIFS= read -r password\nprintf '%s\\n' \"$password\" > \"$SUDO_PASSWORD_CAPTURE\"\nif [ \"${MCP_SHELL_SUDO_PASSWORD+x}\" = x ]; then exit 88; fi\nprintf 'called\\n' >> \"$SUDO_CALL_LOG\"\nprintf 'mock-sudo %s\\n' \"$*\"\n"), 0700))
	t.Setenv("PATH", binDir)
	t.Setenv("SUDO_CALL_LOG", callLog)
	passwordCapture := filepath.Join(t.TempDir(), "sudo-password.txt")
	t.Setenv("SUDO_PASSWORD_CAPTURE", passwordCapture)

	var executeWG sync.WaitGroup
	results := make(chan *mcp.CallToolResult, 2)
	for i := 0; i < 2; i++ {
		executeWG.Add(1)
		go func() {
			defer executeWG.Done()
			result, callErr := h.execute(context.Background(), toolRequest(map[string]interface{}{"request_id": id}))
			if callErr != nil {
				t.Errorf("execute returned Go error: %v", callErr)
				return
			}
			results <- result
		}()
	}
	executeWG.Wait()
	close(results)
	successes, failures := 0, 0
	for result := range results {
		if result.IsError {
			failures++
		} else {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, failures)
	calls, err := os.ReadFile(callLog)
	require.NoError(t, err)
	require.Equal(t, "called\n", string(calls))
	// The fake sudo receives the password through stdin, not command arguments.
	password, err := os.ReadFile(passwordCapture)
	require.NoError(t, err)
	require.Equal(t, "test-password\n", string(password))
}

func TestPrivilegedExecuteRequiresPasswordWithoutClaimingRequest(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("MCP_SHELL_PRIVILEGED_STATE_DIR", stateDir)
	t.Setenv("MCP_SHELL_SUDO_PASSWORD", "")
	h, err := newPrivilegedHandler(time.Second, 1024, zerolog.Nop())
	require.NoError(t, err)
	requested, err := h.request(context.Background(), toolRequest(map[string]interface{}{"command": "echo safe"}))
	require.NoError(t, err)
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(requested.Content[0].(mcp.TextContent).Text), &payload))
	id := payload["request_id"].(string)
	result, err := h.execute(context.Background(), toolRequest(map[string]interface{}{"request_id": id}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	_, err = os.Stat(filepath.Join(stateDir, id+".pending.json"))
	require.NoError(t, err, "missing password must not consume the request")
}

func TestPrivilegedRequestRejectsShellSyntaxAndInterpreters(t *testing.T) {
	t.Setenv("MCP_SHELL_PRIVILEGED_STATE_DIR", t.TempDir())
	h, err := newPrivilegedHandler(time.Second, 1024, zerolog.Nop())
	require.NoError(t, err)

	for _, command := range []string{"echo safe; id", "bash -c id", "python -c print(1)"} {
		result, err := h.request(context.Background(), toolRequest(map[string]interface{}{"command": command}))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.IsError, "command should be rejected: %s", command)
	}
}
