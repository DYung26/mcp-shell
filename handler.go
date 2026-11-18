package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
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
	arguments, _ := request.Params.Arguments.(map[string]interface{})
	if seconds, ok := arguments["background_after_seconds"]; ok {
		waitSeconds, valid := numericSeconds(seconds)
		if !valid || waitSeconds <= 0 || waitSeconds > 300 {
			return mcp.NewToolResultError("background_after_seconds must be between 1 and 300"), nil
		}
		job, err := h.jobs.start(command)
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
		return marshalToolResponse(map[string]interface{}{"job": record, "output": output, "handed_off": record.Status == jobQueued || record.Status == jobRunning})
	}

	result, err := h.executor.execute(ctx, command, useBase64)
	if err != nil {
		h.logger.Error().
			Err(err).
			Str("command", command).
			Msg("Command execution failed")
		return mcp.NewToolResultError(err.Error()), nil
	}

	response := map[string]interface{}{
		"status":           result.Status,
		"exit_code":        result.ExitCode,
		"stdout":           result.Stdout,
		"stderr":           result.Stderr,
		"command":          result.Command,
		"execution_time":   result.ExecutionTime.String(),
		"output_truncated": result.OutputTruncated,
	}

	if result.SecurityInfo != nil {
		response["security_info"] = result.SecurityInfo
	}

	jsonBytes, err := json.Marshal(response)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to marshal response")
		return mcp.NewToolResultError("Failed to marshal result to JSON"), nil
	}

	h.logger.Debug().
		Str("command", command).
		Str("status", result.Status).
		Msg("Request handled successfully")

	return mcp.NewToolResultText(string(jsonBytes)), nil
}

func (h *ShellHandler) runBackground(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command, err := request.RequireString("command")
	if err != nil {
		return mcp.NewToolResultError("Missing 'command' parameter"), nil
	}
	if err := h.validator.validateCommand(command); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Security violation: %s", err.Error())), nil
	}
	job, err := h.jobs.start(command)
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

func (h *ShellHandler) listJobs(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jobs, err := h.jobs.list()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return marshalToolResponse(map[string]interface{}{"jobs": jobs})
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
