package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
)

// Privileged requests are independent, one-shot records. A request moves by
// atomic rename from pending -> executing -> completed, so concurrent agents
// cannot overwrite one another or execute the same request twice.
type privilegedRequest struct {
	ID                string    `json:"request_id"`
	Command           string    `json:"command"`
	Argv              []string  `json:"argv"`
	Reason            string    `json:"reason"`
	CreatedAt         time.Time `json:"created_at"`
	ApprovalStatus    string    `json:"approval_status"`
	AuthorizationMode string    `json:"authorization_mode"`
}

type privilegedHandler struct {
	dir          string
	sudoPassword string
	timeout      time.Duration
	maxOut       int
	logger       zerolog.Logger
}

func newPrivilegedHandler(timeout time.Duration, maxOut int, logger zerolog.Logger) (*privilegedHandler, error) {
	// Capture the service-scoped secret once and remove it from the process
	// environment so commands launched later do not inherit it.
	sudoPassword := os.Getenv("MCP_SHELL_SUDO_PASSWORD")
	_ = os.Unsetenv("MCP_SHELL_SUDO_PASSWORD")
	if strings.ContainsAny(sudoPassword, "\r\n") {
		return nil, fmt.Errorf("MCP_SHELL_SUDO_PASSWORD must not contain newline characters")
	}
	dir := os.Getenv("MCP_SHELL_PRIVILEGED_STATE_DIR")
	if dir == "" {
		if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
			dir = filepath.Join(stateHome, "mcp-shell", "privileged-requests")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve home directory: %w", err)
			}
			dir = filepath.Join(home, ".local", "state", "mcp-shell", "privileged-requests")
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create privileged request state directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("secure privileged request state directory: %w", err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOut <= 0 {
		maxOut = 1 << 20
	}
	return &privilegedHandler{
		dir: dir, sudoPassword: sudoPassword, timeout: timeout, maxOut: maxOut,
		logger: logger.With().Str("component", "privileged").Logger(),
	}, nil
}

func (h *privilegedHandler) path(id, state string) string {
	return filepath.Join(h.dir, id+"."+state+".json")
}

func validRequestID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (h *privilegedHandler) request(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command, err := req.RequireString("command")
	if err != nil || strings.TrimSpace(command) == "" {
		return mcp.NewToolResultError("Missing required 'command' parameter"), nil
	}
	reason, _ := req.RequireString("reason")
	parsed := newCommandUnfurler().unfurl(command)
	if !parsed.Allowed {
		return mcp.NewToolResultError(fmt.Sprintf("Privileged operation rejected: %s", parsed.Reason)), nil
	}
	if isInterpreterExecutable(filepath.Base(parsed.Argv[0])) {
		return mcp.NewToolResultError("Privileged operation cannot invoke a shell or language interpreter; request a direct executable operation"), nil
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("generate request ID: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	r := privilegedRequest{
		ID: id, Command: command, Argv: parsed.Argv, Reason: reason,
		CreatedAt:         time.Now().UTC(),
		ApprovalStatus:    "not_required",
		AuthorizationMode: "approval_disabled_initial_implementation",
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(h.dir, ".request-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, h.path(id, "pending")); err != nil {
		return nil, err
	}
	h.logger.Warn().Str("request_id", id).Str("command", command).Str("reason", reason).
		Msg("Privileged operation requested without human approval (initial mode)")
	return mcp.NewToolResultText(fmt.Sprintf(
		"{\"request_id\":%q,\"status\":\"pending_execution\",\"approval_status\":\"not_required\",\"authorization_mode\":\"approval_disabled_initial_implementation\",\"command\":%q}",
		id, command)), nil
}

func (h *privilegedHandler) execute(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.sudoPassword == "" {
		return mcp.NewToolResultError("MCP_SHELL_SUDO_PASSWORD is not configured for the mcp-shell service"), nil
	}
	id, err := req.RequireString("request_id")
	if err != nil || !validRequestID(id) {
		return mcp.NewToolResultError("Invalid or missing request_id"), nil
	}

	pending := h.path(id, "pending")
	executing := h.path(id, "executing")
	// Rename is atomic on the same filesystem. Exactly one concurrent caller
	// can claim a pending request, including callers in separate server processes.
	if err := os.Rename(pending, executing); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mcp.NewToolResultError("Request does not exist, has already been claimed, or has already been executed"), nil
		}
		return nil, fmt.Errorf("claim privileged request: %w", err)
	}

	data, err := os.ReadFile(executing)
	if err != nil {
		return nil, fmt.Errorf("read claimed request: %w", err)
	}
	var r privilegedRequest
	if err := json.Unmarshal(data, &r); err != nil || r.ID != id || len(r.Argv) == 0 {
		return mcp.NewToolResultError("Stored privileged request is invalid; refusing execution"), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	// No shell is involved. Feed sudo its password over stdin; the secret is
	// never placed in command arguments, request records, or tool output.
	args := append([]string{"-k", "-S", "-p", "", "--"}, r.Argv...)
	cmd := exec.CommandContext(runCtx, "sudo", args...)
	cmd.Stdin = strings.NewReader(h.sudoPassword + "\n")
	cmd.Dir = "/"
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = h.maxOut, h.maxOut
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	runErr := cmd.Run()
	exitCode := 0
	status := "success"
	if runErr != nil {
		status = "error"
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			status = "timeout"
		}
	}
	result := map[string]interface{}{
		"request_id": id, "status": status, "exit_code": exitCode,
		"stdout": stdout.String(), "stderr": stderr.String(),
		"output_truncated": stdout.truncated || stderr.truncated,
		"command":          r.Command, "execution_time": time.Since(started).String(),
		"authorization_mode": r.AuthorizationMode,
	}
	resultData, marshalErr := json.Marshal(result)
	if marshalErr == nil {
		_ = os.WriteFile(h.path(id, "completed"), resultData, 0600)
	}
	_ = os.Remove(executing)
	h.logger.Warn().Str("request_id", id).Str("command", r.Command).
		Str("status", status).Int("exit_code", exitCode).
		Msg("Privileged operation completed")
	if marshalErr != nil {
		return nil, marshalErr
	}
	return mcp.NewToolResultText(string(resultData)), nil
}

type limitedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		b.data = append(b.data, p[:remaining]...)
	}
	if remaining < n {
		b.truncated = true
	}
	// Continue draining the pipe after the output cap so the child process
	// cannot hang or receive an unexpected broken pipe solely due to truncation.
	return n, nil
}

func (b *limitedBuffer) String() string { return string(b.data) }
