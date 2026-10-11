package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var version = "dev"

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
			"Execute shell commands with configurable security constraints. Returns structured JSON with stdout, stderr, exit code and execution metadata.",
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithBoolean(
			"base64",
			mcp.DefaultBool(false),
			mcp.Description(
				"Return stdout/stderr as base64-encoded strings (useful for binary data)",
			),
		),
	)

	s.AddTool(shellTool, shellHandler.handle)

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
