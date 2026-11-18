package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

type ExecutionResult struct {
	Status          string        `json:"status"`
	ExitCode        int           `json:"exit_code"`
	Stdout          string        `json:"stdout"`
	Stderr          string        `json:"stderr"`
	Command         string        `json:"command"`
	ExecutionTime   time.Duration `json:"execution_time"`
	OutputTruncated bool          `json:"output_truncated"`
	SecurityInfo    *SecurityInfo `json:"security_info,omitempty"`
}

type SecurityInfo struct {
	SecurityEnabled bool   `json:"security_enabled"`
	WorkingDir      string `json:"working_dir,omitempty"`
	RunAsUser       string `json:"run_as_user,omitempty"`
	TimeoutApplied  bool   `json:"timeout_applied"`
}

type CommandExecutor struct {
	config   SecurityConfig
	logger   zerolog.Logger
	unfurler *commandUnfurler
}

func newCommandExecutor(cfg SecurityConfig, logger zerolog.Logger) *CommandExecutor {
	return &CommandExecutor{
		config:   cfg,
		logger:   logger.With().Str("component", "executor").Logger(),
		unfurler: newCommandUnfurler(),
	}
}

func (e *CommandExecutor) execute(
	ctx context.Context,
	command string,
	useBase64 bool,
) (*ExecutionResult, error) {
	start := time.Now()

	e.logger.Info().
		Str("command", command).
		Bool("base64", useBase64).
		Msg("Executing command")

	timeout := 30 * time.Second
	if e.config.MaxExecutionTime > 0 {
		timeout = e.config.MaxExecutionTime
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := e.executeSecureCommand(cmdCtx, command, useBase64)
	if err != nil {
		e.logger.Error().
			Err(err).
			Str("command", command).
			Msg("Command execution failed")
		return nil, err
	}

	result.ExecutionTime = time.Since(start)
	result.SecurityInfo = &SecurityInfo{
		SecurityEnabled: e.config.Enabled,
		TimeoutApplied:  true,
	}

	if e.config.WorkingDirectory != "" {
		result.SecurityInfo.WorkingDir = e.config.WorkingDirectory
	}
	if e.config.RunAsUser != "" {
		result.SecurityInfo.RunAsUser = e.config.RunAsUser
	}

	e.logger.Info().
		Str("command", command).
		Str("status", result.Status).
		Int("exit_code", result.ExitCode).
		Dur("execution_time", result.ExecutionTime).
		Msg("Command execution completed")

	return result, nil
}

func (e *CommandExecutor) executeSecureCommand(
	ctx context.Context,
	command string,
	useBase64 bool,
) (*ExecutionResult, error) {
	var stdoutBuf, stderrBuf bytes.Buffer
	result, err := e.executeSecureCommandTo(ctx, command, &stdoutBuf, &stderrBuf, nil)
	if err != nil {
		return nil, err
	}
	if result.OutputTruncated {
		return nil, fmt.Errorf("command output exceeds maximum size limit")
	}
	if useBase64 {
		result.Stdout = base64.StdEncoding.EncodeToString(stdoutBuf.Bytes())
		result.Stderr = base64.StdEncoding.EncodeToString(stderrBuf.Bytes())
	} else {
		result.Stdout = strings.TrimRight(stdoutBuf.String(), "\n")
		result.Stderr = strings.TrimRight(stderrBuf.String(), "\n")
	}
	return result, nil
}

func (e *CommandExecutor) executeSecureCommandTo(
	ctx context.Context,
	command string,
	stdout io.Writer,
	stderr io.Writer,
	onStart func(pid int) error,
) (*ExecutionResult, error) {
	var cmd *exec.Cmd

	// Use secure execution unless legacy shell mode is explicitly enabled
	if e.config.UseShellExecution {
		e.logger.Warn().
			Str("command", command).
			Msg("Using legacy shell execution mode - vulnerable to injection attacks")
		cmd = exec.CommandContext(ctx, "bash", "-c", command)
	} else {
		// Secure execution: parse the command into a literal argv and execute it
		// directly, using the same unfurler the validator used.
		res := e.unfurler.unfurl(command)
		if !res.Allowed {
			e.logger.Error().
				Str("command", command).
				Str("reason", res.Reason).
				Msg("Failed to parse command securely")
			return nil, fmt.Errorf("command parsing failed: %s", res.Reason)
		}

		e.logger.Debug().
			Str("executable", res.Argv[0]).
			Strs("args", res.Argv[1:]).
			Msg("Executing command with direct execution")

		cmd = exec.CommandContext(ctx, res.Argv[0], res.Argv[1:]...)
	}

	if e.config.WorkingDirectory != "" {
		if err := os.MkdirAll(e.config.WorkingDirectory, 0755); err == nil {
			cmd.Dir = e.config.WorkingDirectory
			e.logger.Debug().
				Str("working_dir", e.config.WorkingDirectory).
				Msg("Set working directory")
		}
	}

	if e.config.RunAsUser != "" {
		if err := configureRunAsUser(cmd, e.config.RunAsUser, e.logger); err != nil {
			return nil, fmt.Errorf("configure run_as_user %q: %w", e.config.RunAsUser, err)
		}
	}

	limitedStdout := newLimitedWriter(stdout, e.config.MaxOutputSize)
	limitedStderr := newLimitedWriter(stderr, e.config.MaxOutputSize)
	cmd.Stdout = limitedStdout
	cmd.Stderr = limitedStderr

	configureCommandProcess(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return terminateCommandProcess(cmd.Process.Pid)
	}
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Start()
	if err == nil {
		if onStart != nil {
			if callbackErr := onStart(cmd.Process.Pid); callbackErr != nil {
				_ = terminateCommandProcess(cmd.Process.Pid)
				_ = cmd.Wait()
				return nil, fmt.Errorf("persist command process state: %w", callbackErr)
			}
		}
		err = cmd.Wait()
	}

	exitCode := 0
	status := "success"
	if err != nil {
		status = "error"
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return &ExecutionResult{
		Status:          status,
		ExitCode:        exitCode,
		Command:         command,
		OutputTruncated: limitedStdout.truncated || limitedStderr.truncated,
	}, nil
}

type limitedWriter struct {
	writer    io.Writer
	limit     int
	written   int
	truncated bool
}

func newLimitedWriter(writer io.Writer, limit int) *limitedWriter {
	return &limitedWriter{writer: writer, limit: limit}
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.limit <= 0 {
		n, err := w.writer.Write(data)
		w.written += n
		return n, err
	}
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		w.truncated = true
		if _, err := w.writer.Write(data[:remaining]); err != nil {
			return 0, err
		}
		w.written += remaining
		return len(data), nil
	}
	n, err := w.writer.Write(data)
	w.written += n
	return n, err
}
