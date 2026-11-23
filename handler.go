package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
)

const (
	defaultBackgroundHandoffSeconds = 20
	maxBackgroundHandoffSeconds     = 300
	defaultJobListLimit             = 25
	maxJobListLimit                 = 100
)

type ShellHandler struct {
	validator *SecurityValidator
	executor  *CommandExecutor
	logger    zerolog.Logger
	jobs      *JobManager
}

func newShellHandler(
	validator *SecurityValidator,
	executor *CommandExecutor,
	logger zerolog.Logger,
) *ShellHandler {
	return &ShellHandler{
		validator: validator,
		executor:  executor,
		logger:    logger.With().Str("component", "handler").Logger(),
		jobs:      newJobManager(executor.config, logger),
	}
}

func (h *ShellHandler) handle(
	ctx context.Context,
	request mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	command, err := request.RequireString("command")
	if err != nil {
		h.logger.Error().Err(err).Msg("Missing command parameter")
		return mcp.NewToolResultError("Missing 'command' parameter"), nil
	}

	h.logger.Info().Str("command", command).Msg("Received shell command request")

	if h.validator.isEnabled() {
		h.logger.Info().
			Str("command", command).
			Str("audit", "command_requested").
			Msg("Command execution requested")
	}

	if err := h.validator.validateCommand(command); err != nil {
		h.logger.Warn().
			Err(err).
			Str("command", command).
			Msg("Security validation failed")
		return mcp.NewToolResultError(fmt.Sprintf("Security violation: %s", err.Error())), nil
	}

	useBase64 := request.GetBool("base64", false)
	waitSeconds, err := backgroundHandoffSeconds(toolArguments(request))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	executionTimeout := h.executor.config.MaxExecutionTime
	if executionTimeout <= 0 {
		executionTimeout = defaultCommandExecutionTimeout
	}
	job, err := h.jobs.startShellExec(command, executionTimeout)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	job, err = h.jobs.wait(ctx, job.ID, time.Duration(waitSeconds)*time.Second)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	record, output, err := h.jobs.get(job.ID, 0, 0, useBase64)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if record.Status == jobQueued || record.Status == jobRunning {
		return marshalToolResponse(map[string]interface{}{"job": record, "output": output, "handed_off": true})
	}
	record, output, err = h.jobs.getComplete(record.ID, useBase64)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := h.jobs.delete(record.ID); err != nil {
		h.logger.Warn().Err(err).Str("job_id", record.ID).Msg("Could not remove completed shell_exec job")
	}
	if record.Error != "" {
		return mcp.NewToolResultError(record.Error), nil
	}
	if record.OutputTruncated {
		return mcp.NewToolResultError("command output exceeds maximum size limit"), nil
	}

	exitCode := -1
	if record.ExitCode != nil {
		exitCode = *record.ExitCode
	}
	status := "success"
	if record.Status != jobSucceeded {
		status = "error"
	}
	stdout := output.Stdout
	stderr := output.Stderr
	if !useBase64 {
		stdout = strings.TrimRight(stdout, "\n")
		stderr = strings.TrimRight(stderr, "\n")
	}
	response := map[string]interface{}{
		"status":           status,
		"exit_code":        exitCode,
		"stdout":           stdout,
		"stderr":           stderr,
		"command":          record.Command,
		"execution_time":   record.ExecutionDuration.String(),
		"output_truncated": record.OutputTruncated,
	}
	if record.Error != "" {
		response["error"] = record.Error
	}
	response["security_info"] = h.securityInfo()

	h.logger.Debug().
		Str("command", command).
		Str("status", status).
		Msg("Request handled successfully")

	return marshalToolResponse(response)
}

func (h *ShellHandler) runBackground(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command, err := request.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError("Missing 'command' parameter"), nil
	}
	if err := h.validator.validateCommand(command); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Security violation: %s", err.Error())), nil
	}
	job, err := h.jobs.start(command, 0)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(map[string]interface{}{"job": job})
}

func (h *ShellHandler) getJob(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := request.RequireString("job_id")
	if err != nil {
		return mcp.NewToolResultError("Missing 'job_id' parameter"), nil
	}
	stdoutOffset, err := numericOffset(toolArguments(request), "stdout_offset")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	stderrOffset, err := numericOffset(toolArguments(request), "stderr_offset")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	job, output, err := h.jobs.get(id, stdoutOffset, stderrOffset, request.GetBool("base64", false))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(map[string]interface{}{"job": job, "output": output})
}

func (h *ShellHandler) listJobs(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	arguments := toolArguments(request)
	limit := defaultJobListLimit
	if value, exists := arguments["limit"]; exists {
		parsed, err := numericOffset(map[string]interface{}{"limit": value}, "limit")
		if err != nil || parsed < 1 || parsed > maxJobListLimit {
			return mcp.NewToolResultError(fmt.Sprintf("limit must be between 1 and %d", maxJobListLimit)), nil
		}
		limit = int(parsed)
	}

	offset, err := numericOffset(arguments, "offset")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	status := ""
	if value, exists := arguments["status"]; exists {
		statusValue, valid := value.(string)
		if !valid || (statusValue != "" && !isJobStatus(statusValue)) {
			return mcp.NewToolResultError("status must be queued, running, succeeded, failed, cancelled, or timed_out"), nil
		}
		status = statusValue
	}
	commandContains := ""
	if value, exists := arguments["command_contains"]; exists {
		commandValue, valid := value.(string)
		if !valid {
			return mcp.NewToolResultError("command_contains must be a string"), nil
		}
		commandContains = commandValue
	}

	result, err := h.jobs.listPage(jobListOptions{
		Limit:           limit,
		Offset:          offset,
		Status:          status,
		CommandContains: commandContains,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(result)
}

func backgroundHandoffSeconds(arguments map[string]interface{}) (int, error) {
	value, exists := arguments["background_after_seconds"]
	if !exists {
		return defaultBackgroundHandoffSeconds, nil
	}
	seconds, valid := numericSeconds(value)
	if !valid || seconds < 1 || seconds > maxBackgroundHandoffSeconds {
		return 0, fmt.Errorf("background_after_seconds must be between 1 and %d", maxBackgroundHandoffSeconds)
	}
	return seconds, nil
}

func (h *ShellHandler) securityInfo() *SecurityInfo {
	info := &SecurityInfo{
		SecurityEnabled: h.executor.config.Enabled,
		TimeoutApplied:  true,
	}
	if h.executor.config.WorkingDirectory != "" {
		info.WorkingDir = h.executor.config.WorkingDirectory
	}
	if h.executor.config.RunAsUser != "" {
		info.RunAsUser = h.executor.config.RunAsUser
	}
	return info
}

func isJobStatus(status string) bool {
	switch status {
	case jobQueued, jobRunning, jobSucceeded, jobFailed, jobCancelled, jobTimedOut:
		return true
	default:
		return false
	}
}

func (h *ShellHandler) cancelJob(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := request.RequireString("job_id")
	if err != nil {
		return mcp.NewToolResultError("Missing 'job_id' parameter"), nil
	}
	job, err := h.jobs.cancel(id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(map[string]interface{}{"job": job})
}

func marshalToolResponse(value interface{}) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return mcp.NewToolResultError("Failed to marshal job response: " + err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func numericSeconds(value interface{}) (int, bool) {
	switch number := value.(type) {
	case float64:
		if number != float64(int(number)) {
			return 0, false
		}
		return int(number), true
	case int:
		return number, true
	case string:
		parsed, err := strconv.Atoi(number)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func toolArguments(request mcp.CallToolRequest) map[string]interface{} {
	arguments, _ := request.Params.Arguments.(map[string]interface{})
	return arguments
}

func numericOffset(arguments map[string]interface{}, key string) (int64, error) {
	value, ok := arguments[key]
	if !ok {
		return 0, nil
	}
	switch number := value.(type) {
	case float64:
		if number < 0 || number != float64(int64(number)) {
			return 0, fmt.Errorf("%s must be a non-negative integer", key)
		}
		return int64(number), nil
	case int:
		if number < 0 {
			return 0, fmt.Errorf("%s must be non-negative", key)
		}
		return int64(number), nil
	default:
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
}

func (h *ShellHandler) deleteJob(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := request.RequireString("job_id")
	if err != nil {
		return mcp.NewToolResultError("Missing 'job_id' parameter"), nil
	}
	if err := h.jobs.delete(id); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(map[string]interface{}{"job_id": id, "deleted": true})
}
