package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyHandler_listAllowedCommands(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	handler := newPolicyHandler(SecurityConfig{
		Enabled:            true,
		UseShellExecution:  false,
		AllowedExecutables: []string{"ls", "/bin/cat", "git"},
	}, logger)

	result, err := handler.listAllowedCommands(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var response AllowedCommandsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &response))

	assert.True(t, response.SecurityEnabled)
	assert.False(t, response.UseShellExecution)
	assert.Equal(t, []string{"ls", "/bin/cat", "git"}, response.AllowedCommands)
}

func TestPolicyHandler_listAllowedCommands_returnsCopy(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	config := SecurityConfig{
		Enabled:            true,
		AllowedExecutables: []string{"ls", "cat"},
	}
	handler := newPolicyHandler(config, logger)

	result, err := handler.listAllowedCommands(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)

	config.AllowedExecutables[0] = "modified"

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var response AllowedCommandsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &response))
	assert.Equal(t, "ls", response.AllowedCommands[0])
}

func TestPolicyHandler_listAllowedCommands_usesLegacyAllowlistInShellMode(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))
	handler := newPolicyHandler(SecurityConfig{
		Enabled:            true,
		UseShellExecution:  true,
		AllowedCommands:    []string{"bash", "git", "ls"},
		AllowedExecutables: []string{"echo"},
	}, logger)

	result, err := handler.listAllowedCommands(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.NotNil(t, result)

	textContent, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var response AllowedCommandsResponse
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &response))
	assert.True(t, response.UseShellExecution)
	assert.Equal(t, []string{"bash", "git", "ls"}, response.AllowedCommands)
}
