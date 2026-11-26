package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var version = "dev"

func init() {
	if os.Getenv("MCP_SHELL_JOB_WORKER") != "1" {
		return
	}
	config, err := loadConfig()
	if err != nil {
		os.Exit(2)
	}
	logger, err := newLogger(config.Logging)
	if err != nil {
		os.Exit(2)
	}
	manager := newJobManager(config.Security, logger)
	if err := manager.runWorker(os.Getenv("MCP_SHELL_JOB_ID")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	if version != "dev" {
		cfg.Server.Version = version
	}

	log, err := newLogger(cfg.Logging)
	if err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}

	configFile := os.Getenv("MCP_SHELL_SEC_CONFIG_FILE")
	switch {
	case configFile != "":
		log.Info().Str("config_file", configFile).Msg("Loading security config from file")
	case cfg.Security.Enabled:
		log.Info().Msg("No security config file specified, using built-in secure defaults")
	default:
		log.Warn().Msg("SECURITY DISABLED via MCP_SHELL_ALLOW_UNSAFE - all commands run unrestricted")
	}

	log.Info().
		Str("server_name", cfg.Server.Name).
		Str("version", cfg.Server.Version).
		Str("log_level", cfg.Logging.Level).
		Str("log_format", cfg.Logging.Format).
		Bool("security_enabled", cfg.Security.Enabled).
		Msg("Configuration loaded")

	if cfg.Security.Enabled {
		log.Info().
			Str("working_dir", cfg.Security.WorkingDirectory).
			Dur("max_execution_time", cfg.Security.MaxExecutionTime).
			Int("max_output_size", cfg.Security.MaxOutputSize).
			Int("allowed_commands", len(cfg.Security.AllowedCommands)).
			Int("blocked_commands", len(cfg.Security.BlockedCommands)).
			Int("blocked_patterns", len(cfg.Security.BlockedPatterns)).
			Bool("audit_log", cfg.Security.AuditLog).
			Msg("Security configuration")

		log.Debug().
			Strs("allowed_commands", cfg.Security.AllowedCommands).
			Msg("Allowed commands list")

		log.Debug().
			Strs("blocked_commands", cfg.Security.BlockedCommands).
			Msg("Blocked commands list")

		log.Debug().
			Strs("blocked_patterns", cfg.Security.BlockedPatterns).
			Msg("Blocked patterns list")
	}

	validator := newSecurityValidator(cfg.Security, log)
	executor := newCommandExecutor(cfg.Security, log)
	shellHandler := newShellHandler(validator, executor, log)
	policyHandler := newPolicyHandler(cfg.Security, log)
	privileged, err := newPrivilegedHandler(cfg.Security.MaxExecutionTime, cfg.Security.MaxOutputSize, log)
	if err != nil {
		return fmt.Errorf("failed to initialize privileged-operation handler: %w", err)
	}

	s := server.NewMCPServer(
		cfg.Server.Name,
		cfg.Server.Version,
		server.WithToolCapabilities(false),
	)

	shellTool := mcp.NewTool(
		"shell_exec",
		mcp.WithDescription(
			"Execute a command and wait synchronously for up to 20 seconds by default. If it is still running, return a durable job_id and current output; use shell_get_job to continue retrieving output. max_execution_time limits this synchronous phase; handed-off jobs use max_background_execution_time.",
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithNumber("background_after_seconds", mcp.Description("Handoff threshold in seconds (default 20, maximum 300). Return the command result if it finishes within this period; otherwise return a job_id and current output. The handed-off job then runs up to max_background_execution_time.")),
		mcp.WithBoolean(
			"base64",
			mcp.DefaultBool(false),
			mcp.Description(
				"Return stdout/stderr as base64-encoded strings (useful for binary data)",
			),
		),
	)

	s.AddTool(shellTool, shellHandler.handle)

	backgroundTool := mcp.NewTool(
		"shell_run_background",
		mcp.WithDescription("Start a command as a durable background job and return immediately with its job_id. Use shell_get_job to retrieve status and incremental output. When the retention limit is reached, the oldest completed jobs are pruned automatically; active jobs are preserved."),
		mcp.WithString("command", mcp.Required(), mcp.Description("Command to execute under the configured shell security policy.")),
	)
	s.AddTool(backgroundTool, shellHandler.runBackground)

	getJobTool := mcp.NewTool(
		"shell_get_job",
		mcp.WithDescription("Get a background job's status and the next output chunk. Pass returned offsets to retrieve subsequent chunks. The response includes can_delete=true when the job is complete and may be removed with shell_delete_job."),
		mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID returned by shell_run_background or shell_exec handoff.")),
		mcp.WithNumber("stdout_offset", mcp.Description("Byte offset for the next stdout chunk; default 0.")),
		mcp.WithNumber("stderr_offset", mcp.Description("Byte offset for the next stderr chunk; default 0.")),
		mcp.WithBoolean("base64", mcp.DefaultBool(false), mcp.Description("Base64-encode returned output chunks.")),
	)
	s.AddTool(getJobTool, shellHandler.getJob)

	listJobsTool := mcp.NewTool(
		"shell_list_jobs",
		mcp.WithDescription("List retained background jobs, newest first. Results are paginated and can be filtered by status or command text."),
		mcp.WithNumber("limit", mcp.Description("Number of jobs to return (default 25, maximum 100).")),
		mcp.WithNumber("offset", mcp.Description("Number of matching jobs to skip (default 0).")),
		mcp.WithString("status", mcp.Description("Optional exact status filter: queued, running, succeeded, failed, cancelled, or timed_out.")),
		mcp.WithString("command_contains", mcp.Description("Optional case-insensitive substring filter for the command.")),
	)
	s.AddTool(listJobsTool, shellHandler.listJobs)

	cancelJobTool := mcp.NewTool(
		"shell_cancel_job",
		mcp.WithDescription("Cancel a queued or running background job. On Unix, command and worker process groups are terminated; Windows targets the direct processes."),
		mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID to cancel.")),
	)
	s.AddTool(cancelJobTool, shellHandler.cancelJob)

	deleteJobTool := mcp.NewTool(
		"shell_delete_job",
		mcp.WithDescription("Delete a completed background job and its stored output. Active jobs must be cancelled first."),
		mcp.WithString("job_id", mcp.Required(), mcp.Description("Completed job ID to delete.")),
	)
	s.AddTool(deleteJobTool, shellHandler.deleteJob)

	allowedCommandsTool := mcp.NewTool(
		"list_allowed_commands",
		mcp.WithDescription(
			"List the effective executable allowlist and shell security mode currently used by mcp-shell.",
		),
	)

	s.AddTool(allowedCommandsTool, policyHandler.listAllowedCommands)

	requestPrivilegedTool := mcp.NewTool(
		"request_privileged_operation",
		mcp.WithDescription("Create an independent, one-shot request for a privileged operation. This initial implementation does not require human approval; execute the returned request_id with execute_privileged_operation. Commands must be a single direct executable invocation, with no shell operators or interpreters. Execution uses MCP_SHELL_SUDO_PASSWORD to authenticate sudo non-interactively."),
		mcp.WithString("command", mcp.Required(), mcp.Description("One direct command and literal arguments to run through sudo; no shell syntax.")),
		mcp.WithString("reason", mcp.Description("Short explanation of why the privileged operation is needed.")),
	)
	s.AddTool(requestPrivilegedTool, privileged.request)

	executePrivilegedTool := mcp.NewTool(
		"execute_privileged_operation",
		mcp.WithDescription("Execute exactly one pending privileged operation created by request_privileged_operation. Each request ID is independent and can be claimed only once; no arbitrary command can be supplied at execution time."),
		mcp.WithString("request_id", mcp.Required(), mcp.Description("The request_id returned by request_privileged_operation.")),
	)
	s.AddTool(executePrivilegedTool, privileged.execute)

	log.Info().Msg("MCP server initialized, serving on stdio")

	if err := server.ServeStdio(s); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}
