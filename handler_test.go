package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackgroundHandoffSecondsDefaultsAndValidates(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]interface{}
		want      int
		wantError bool
	}{
		{name: "default threshold", arguments: map[string]interface{}{}, want: defaultBackgroundHandoffSeconds},
		{name: "custom threshold", arguments: map[string]interface{}{"background_after_seconds": float64(5)}, want: 5},
		{name: "zero is rejected", arguments: map[string]interface{}{"background_after_seconds": float64(0)}, wantError: true},
		{name: "fraction is rejected", arguments: map[string]interface{}{"background_after_seconds": 1.5}, wantError: true},
		{name: "values above maximum are rejected", arguments: map[string]interface{}{"background_after_seconds": float64(maxBackgroundHandoffSeconds + 1)}, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := backgroundHandoffSeconds(test.arguments)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestShellHandler_handleAppliesConfiguredExecutionTimeout(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "security.yaml")
	configData := []byte("security:\n  enabled: true\n  use_shell_execution: false\n  allowed_executables: [sleep]\n  max_execution_time: 100ms\n  max_background_execution_time: 5s\n  working_directory: /tmp\n")
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	t.Setenv("MCP_SHELL_JOB_DIR", filepath.Join(root, "jobs"))
	t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)

	logger := zerolog.New(zerolog.NewTestWriter(t))
	config := SecurityConfig{
		Enabled:                    true,
		AllowedExecutables:         []string{"sleep"},
		MaxExecutionTime:           100 * time.Millisecond,
		MaxBackgroundExecutionTime: 5 * time.Second,
	}
	handler := newShellHandler(
		newSecurityValidator(config, logger),
		newCommandExecutor(config, logger),
		logger,
	)
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"command":                  "sleep 1",
		"background_after_seconds": float64(2),
	}

	result, err := handler.handle(context.Background(), request)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(mcp.TextContent).Text, "execution exceeded 100ms")
	entries, err := os.ReadDir(filepath.Join(root, "jobs"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestShellHandler_handleHandsOffLongRunningCommand(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "security.yaml")
	configData := []byte("security:\n  enabled: true\n  use_shell_execution: false\n  allowed_executables: [sleep]\n  max_execution_time: 5s\n  max_background_execution_time: 10s\n  max_output_size: 1024\n  working_directory: /tmp\n")
	require.NoError(t, os.WriteFile(configPath, configData, 0600))
	t.Setenv("MCP_SHELL_JOB_DIR", filepath.Join(root, "jobs"))
	t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)

	logger := zerolog.New(zerolog.NewTestWriter(t))
	config := SecurityConfig{
		Enabled:                    true,
		AllowedExecutables:         []string{"sleep"},
		MaxExecutionTime:           5 * time.Second,
		MaxBackgroundExecutionTime: 10 * time.Second,
		MaxOutputSize:              1024,
		WorkingDirectory:           "/tmp",
	}
	handler := newShellHandler(
		newSecurityValidator(config, logger),
		newCommandExecutor(config, logger),
		logger,
	)
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"command":                  "sleep 2",
		"background_after_seconds": float64(1),
	}

	result, err := handler.handle(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	var response map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &response))
	require.Equal(t, true, response["handed_off"])
	job := response["job"].(map[string]interface{})
	jobID := job["job_id"].(string)
	require.NotEmpty(t, jobID)

	completed, err := handler.jobs.wait(context.Background(), jobID, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, jobSucceeded, completed.Status)
	_, output, err := handler.jobs.getComplete(jobID, false)
	require.NoError(t, err)
	require.Empty(t, output.Stdout)
	require.NoError(t, handler.jobs.delete(jobID))
}

func TestShellHandler_listJobsReturnsFilteredPage(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	manager := &JobManager{root: t.TempDir(), logger: logger}
	handler := &ShellHandler{jobs: manager}
	createdAt := time.Now().UTC()
	jobs := []*JobRecord{
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0", Command: "go test ./...", Status: jobSucceeded, CreatedAt: createdAt},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f1", Command: "git status", Status: jobFailed, CreatedAt: createdAt.Add(-time.Minute)},
		{ID: "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f2", Command: "go test ./pkg", Status: jobSucceeded, CreatedAt: createdAt.Add(-2 * time.Minute)},
	}
	for _, job := range jobs {
		require.NoError(t, manager.write(job))
	}

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{
		"limit":            float64(1),
		"offset":           float64(0),
		"status":           jobSucceeded,
		"command_contains": "go test",
	}
	result, err := handler.listJobs(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	var page jobListResult
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &page))
	require.Equal(t, 2, page.Total)
	require.Equal(t, 1, page.Limit)
	require.True(t, page.HasMore)
	require.Len(t, page.Jobs, 1)
	require.Equal(t, jobs[0].ID, page.Jobs[0].ID)
}

func TestShellHandler_getJobReportsWhetherCompletedJobCanBeDeleted(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	manager := &JobManager{root: t.TempDir(), logger: logger}
	handler := &ShellHandler{jobs: manager}
	job := &JobRecord{
		ID:        "f60fbd50-9e16-4f3d-989c-0c7b05e2a4f0",
		Status:    jobRunning,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, manager.write(job))

	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]interface{}{"job_id": job.ID}
	result, err := handler.getJob(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, `"can_delete":false`)

	job.Status = jobSucceeded
	require.NoError(t, manager.write(job))
	result, err = handler.getJob(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, `"can_delete":true`)
}

func TestShellHandler_handle_secure_mode(t *testing.T) {
	t.Setenv("MCP_SHELL_JOB_DIR", t.TempDir())
	logger := zerolog.New(zerolog.NewTestWriter(t))
	ctx := context.Background()

	tests := []struct {
		name            string
		config          SecurityConfig
		requestArgs     map[string]interface{}
		expectError     bool
		expectErrorText string
	}{
		{
			name: "secure mode allows safe command",
			config: SecurityConfig{
				Enabled:            true,
				UseShellExecution:  false,
				AllowedExecutables: []string{"echo", "pwd"},
				MaxExecutionTime:   time.Second * 5,
			},
			requestArgs: map[string]interface{}{
				"command": "echo hello world",
				"base64":  false,
			},
			expectError: false,
		},
		{
			name: "secure mode blocks dangerous command",
			config: SecurityConfig{
				Enabled:            true,
				UseShellExecution:  false,
				AllowedExecutables: []string{"echo", "pwd"},
			},
			requestArgs: map[string]interface{}{
				"command": "rm -rf /",
			},
			expectError:     true,
			expectErrorText: "not in allowed list",
		},
		{
			name: "secure mode blocks injection attempt",
			config: SecurityConfig{
				Enabled:            true,
				UseShellExecution:  false,
				AllowedExecutables: []string{"echo"},
			},
			requestArgs: map[string]interface{}{
				"command": "echo $($(echo -n c; echo -n h; echo -n m; echo -n o; echo -n d))",
			},
			expectError:     true,
			expectErrorText: "not in allowed list",
		},
		{
			name: "missing command parameter",
			config: SecurityConfig{
				Enabled: false,
			},
			requestArgs: map[string]interface{}{
				"base64": false,
			},
			expectError:     true,
			expectErrorText: "Missing 'command' parameter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := newSecurityValidator(tt.config, logger)
			executor := newCommandExecutor(tt.config, logger)
			handler := newShellHandler(validator, executor, logger)

			// Create MCP request using the arguments map
			request := mcp.CallToolRequest{}
			request.Params.Arguments = tt.requestArgs
			request.Params.Name = "shell_exec"

			result, err := handler.handle(ctx, request)

			require.NoError(t, err, "Handler should not return error, but result should contain error")
			require.NotNil(t, result)

			if tt.expectError {
				// Check if result contains error
				assert.True(t, result.IsError)
			} else {
				// Check if result is successful
				assert.False(t, result.IsError)
			}
		})
	}
}

func TestShellHandler_vulnerability_prevention_integration(t *testing.T) {
	t.Setenv("MCP_SHELL_JOB_DIR", t.TempDir())
	logger := zerolog.New(zerolog.NewTestWriter(t))
	ctx := context.Background()

	// Test the exact vulnerability from VULN.md
	vulnerabilityRequest := mcp.CallToolRequest{}
	vulnerabilityRequest.Params.Arguments = map[string]interface{}{
		"command": "echo $($(echo -n c; echo -n h; echo -n m; echo -n o; echo -n d))",
		"base64":  false,
	}

	t.Run("secure_mode_blocks_vuln_md_example", func(t *testing.T) {
		config := SecurityConfig{
			Enabled:            true,
			UseShellExecution:  false,
			AllowedExecutables: []string{"echo", "ls", "pwd"},
		}

		validator := newSecurityValidator(config, logger)
		executor := newCommandExecutor(config, logger)
		handler := newShellHandler(validator, executor, logger)

		result, err := handler.handle(ctx, vulnerabilityRequest)
		require.NoError(t, err)

		// Should be blocked at validation stage
		assert.True(t, result.IsError, "Secure mode should block the injection attempt")
	})

	t.Run("legacy_mode_vulnerable_without_blocks", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "security.yaml")
		configData := []byte("security:\n  enabled: true\n  use_shell_execution: true\n  max_execution_time: 1s\n")
		require.NoError(t, os.WriteFile(configPath, configData, 0600))
		t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)
		config := SecurityConfig{
			Enabled:           true,
			UseShellExecution: true,
			// No blocks - vulnerable
			MaxExecutionTime: time.Second * 1,
		}

		validator := newSecurityValidator(config, logger)
		executor := newCommandExecutor(config, logger)
		handler := newShellHandler(validator, executor, logger)

		result, err := handler.handle(ctx, vulnerabilityRequest)
		require.NoError(t, err)

		// This demonstrates the vulnerability - legacy mode allows dangerous commands
		// In a real attack, this would execute the obfuscated chmod
		t.Logf("Legacy mode result - IsError: %v", result.IsError)
	})

	t.Run("legacy_mode_with_proper_blocks", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "security.yaml")
		configData := []byte("security:\n  enabled: true\n  use_shell_execution: true\n  max_execution_time: 1s\n  blocked_commands:\n    - chmod\n")
		require.NoError(t, os.WriteFile(configPath, configData, 0600))
		t.Setenv("MCP_SHELL_SEC_CONFIG_FILE", configPath)
		config := SecurityConfig{
			Enabled:           true,
			UseShellExecution: true,
			BlockedCommands:   []string{"chmod"}, // This should catch the obfuscated chmod
		}

		validator := newSecurityValidator(config, logger)
		executor := newCommandExecutor(config, logger)
		handler := newShellHandler(validator, executor, logger)

		result, err := handler.handle(ctx, vulnerabilityRequest)
		require.NoError(t, err)

		// This demonstrates the vulnerability - legacy mode cannot detect obfuscated commands
		// even with keyword blocking, since "chmod" doesn't appear literally
		assert.False(t, result.IsError, "Legacy mode with blocks still vulnerable to obfuscation")
	})
}

func TestShellHandler_base64_encoding(t *testing.T) {
	t.Setenv("MCP_SHELL_JOB_DIR", t.TempDir())
	logger := zerolog.New(zerolog.NewTestWriter(t))
	ctx := context.Background()

	config := SecurityConfig{
		Enabled:            true,
		UseShellExecution:  false,
		AllowedExecutables: []string{"echo"},
		MaxExecutionTime:   time.Second * 5,
	}

	validator := newSecurityValidator(config, logger)
	executor := newCommandExecutor(config, logger)
	handler := newShellHandler(validator, executor, logger)

	tests := []struct {
		name    string
		base64  bool
		command string
	}{
		{
			name:    "without base64 encoding",
			base64:  false,
			command: "echo hello world",
		},
		{
			name:    "with base64 encoding",
			base64:  true,
			command: "echo hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := mcp.CallToolRequest{}
			request.Params.Arguments = map[string]interface{}{
				"command": tt.command,
				"base64":  tt.base64,
			}

			result, err := handler.handle(ctx, request)
			require.NoError(t, err)
			assert.False(t, result.IsError, "Base64 encoding test should succeed")
		})
	}
}

// Test direct security validation and execution without MCP wrapper
func TestShellHandler_direct_security_tests(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	t.Run("secure_execution_blocks_injection", func(t *testing.T) {
		config := SecurityConfig{
			Enabled:            true,
			UseShellExecution:  false,
			AllowedExecutables: []string{"echo"},
		}

		validator := newSecurityValidator(config, logger)
		executor := newCommandExecutor(config, logger)

		// Test validation - should pass (this is the vulnerability)
		err := validator.validateCommand("echo $(rm -rf /)")
		assert.Error(t, err, "Should block command with shell metacharacters")

		// Test parsing - the executor's unfurler rejects it as well
		res := executor.unfurler.unfurl("echo $(rm -rf /)")
		assert.False(t, res.Allowed, "Executor should reject command with command substitution")
	})

	t.Run("legacy_execution_allows_injection", func(t *testing.T) {
		config := SecurityConfig{
			Enabled:           true,
			UseShellExecution: true,
			// No restrictions - vulnerable
		}

		validator := newSecurityValidator(config, logger)

		// Test validation - should pass (this is the vulnerability)
		err := validator.validateCommand("echo $(rm -rf /)")
		assert.NoError(t, err, "Legacy mode without blocks allows dangerous commands")
	})

	t.Run("VULN_MD_example_security_comparison", func(t *testing.T) {
		vulnCommand := "echo $($(echo -n c; echo -n h; echo -n m; echo -n o; echo -n d))"

		// Secure mode
		secureConfig := SecurityConfig{
			Enabled:            true,
			UseShellExecution:  false,
			AllowedExecutables: []string{"echo"},
		}
		secureValidator := newSecurityValidator(secureConfig, logger)
		err := secureValidator.validateCommand(vulnCommand)
		assert.Error(t, err, "Secure mode should block VULN.md example")

		// Legacy mode without proper blocks (vulnerable)
		vulnerableConfig := SecurityConfig{
			Enabled:           true,
			UseShellExecution: true,
		}
		vulnerableValidator := newSecurityValidator(vulnerableConfig, logger)
		err = vulnerableValidator.validateCommand(vulnCommand)
		assert.NoError(t, err, "Legacy mode without blocks is vulnerable")

		// Legacy mode with proper blocks - still vulnerable to obfuscated commands
		protectedConfig := SecurityConfig{
			Enabled:           true,
			UseShellExecution: true,
			BlockedCommands:   []string{"chmod"},
		}
		protectedValidator := newSecurityValidator(protectedConfig, logger)
		err = protectedValidator.validateCommand(vulnCommand)
		assert.NoError(t, err, "Legacy mode cannot detect obfuscated commands even with blocks")
	})
}
