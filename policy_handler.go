package main

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
)

type AllowedCommandsResponse struct {
	SecurityEnabled   bool     `json:"security_enabled"`
	UseShellExecution bool     `json:"use_shell_execution"`
	AllowedCommands   []string `json:"allowed_commands"`
}

type PolicyHandler struct {
	config SecurityConfig
	logger zerolog.Logger
}

func newPolicyHandler(config SecurityConfig, logger zerolog.Logger) *PolicyHandler {
	return &PolicyHandler{
		config: config,
		logger: logger.With().Str("component", "policy-handler").Logger(),
	}
}

func (h *PolicyHandler) listAllowedCommands(
	_ context.Context,
	_ mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	allowedCommands := h.config.AllowedExecutables
	if h.config.UseShellExecution {
		allowedCommands = h.config.AllowedCommands
	}

	response := AllowedCommandsResponse{
		SecurityEnabled:   h.config.Enabled,
		UseShellExecution: h.config.UseShellExecution,
		AllowedCommands:   append([]string(nil), allowedCommands...),
	}

	jsonBytes, err := json.Marshal(response)
	if err != nil {
		h.logger.Error().Err(err).Msg("Failed to marshal allowed commands response")
		return mcp.NewToolResultError("Failed to marshal allowed commands to JSON"), nil
	}

	h.logger.Debug().
		Int("allowed_commands", len(response.AllowedCommands)).
		Bool("security_enabled", response.SecurityEnabled).
		Bool("use_shell_execution", response.UseShellExecution).
		Msg("Allowed command list requested")

	return mcp.NewToolResultText(string(jsonBytes)), nil
}
