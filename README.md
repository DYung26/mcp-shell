# mcp-shell

[![Trust Score](https://archestra.ai/mcp-catalog/api/badge/quality/sonirico/mcp-shell)](https://archestra.ai/mcp-catalog/sonirico__mcp-shell)
[![glama](https://glama.ai/mcp/servers/@sonirico/mcp-shell/badge)](https://glama.ai/mcp/servers/@sonirico/mcp-shell)

MCP server that runs shell commands. Your LLM gets a tool; you get control over what runs and how.

Built on [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go). Written in Go.

---

## Run it

**Docker** (easiest):

```bash
docker run -it --rm -v /tmp/mcp-workspace:/tmp/mcp-workspace sonirico/mcp-shell:latest
```

**From source**:

```bash
git clone https://github.com/sonirico/mcp-shell && cd mcp-shell
make install
mcp-shell
```

---

## Configure it

**Secure mode is the default.** With no config file, `mcp-shell` boots in secure
mode restricted to a narrow allowlist of read-only utilities (`ls`, `cat`,
`grep`, `find`, `head`, `tail`, ...). You only need a config file to widen or
change that policy. To run fully unrestricted you must opt in explicitly:

```bash
MCP_SHELL_ALLOW_UNSAFE=true mcp-shell   # disables all validation - do not use in production
```

To customize the policy, point to a YAML config:

```bash
export MCP_SHELL_SEC_CONFIG_FILE=/path/to/security.yaml
mcp-shell
```

**Secure mode** (recommended) — no shell interpretation, executable allowlist only:

```yaml
security:
  enabled: true
  use_shell_execution: false
  allowed_executables:
    - ls
    - cat
    - grep
    - find
    - echo
  # WARNING: never add shell/language interpreters (bash, sh, python, perl,
  # ruby, node) or alias-capable tools (git) here - the interpreter executes
  # whatever it is handed, bypassing secure mode entirely. mcp-shell warns at
  # startup if it finds one.
  blocked_patterns:          # optional: restrict args on allowed commands
    - '(^|\s)remote\s+(-v|--verbose)(\s|$)'
  max_execution_time: 30s
  max_background_execution_time: 24h
  max_output_size: 1048576
  working_directory: /tmp/mcp-workspace
  audit_log: true
```

**Legacy mode** — shell execution, allowlist/blocklist by command string (vulnerable to injection if not careful):

```yaml
security:
  enabled: true
  use_shell_execution: true
  allowed_commands: [ls, cat, grep, echo]
  blocked_patterns: ['rm\s+-rf', 'sudo\s+']
  max_execution_time: 30s
  audit_log: true
```

---

## Wire it up

**Claude Desktop** — add to your MCP config:

```json
{
  "mcpServers": {
    "shell": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "sonirico/mcp-shell:latest"],
      "env": { "MCP_SHELL_LOG_LEVEL": "info" }
    }
  }
}
```

For custom config, mount the file and set the env:

```json
{
  "command": "docker",
  "args": ["run", "--rm", "-i", "-v", "/path/to/security.yaml:/etc/mcp-shell/security.yaml", "-e", "MCP_SHELL_SEC_CONFIG_FILE=/etc/mcp-shell/security.yaml", "sonirico/mcp-shell:latest"]
}
```

---

## Tool API

### `shell_exec`

| Parameter | Type | Description |
|-----------|------|-------------|
| `command` | string | Shell command to run (required) |
| `base64` | boolean | Encode stdout/stderr as base64 (default: false) |
| `background_after_seconds` | number | Handoff threshold in seconds (default 20, range 1-300). The call waits for the command result up to this threshold, then returns a `job_id` and current output if it is still running. |

Response includes `status`, `exit_code`, `stdout`, `stderr`, `command`, `execution_time`, `output_truncated`, and optional `security_info`.

### `shell_run_background`

Starts a command as a detached background job and immediately returns its `job_id`. The command is validated against the same security policy as `shell_exec`.

### `shell_get_job`

Retrieves job status and the next stdout/stderr chunks. Required parameter: `job_id`. Optional `stdout_offset` and `stderr_offset` are byte offsets returned by the previous response; pass them back unchanged to retrieve only new output. Optional `base64` encodes output chunks.

Job states are `queued`, `running`, `succeeded`, `failed`, `cancelled`, and `timed_out`. Results include exit code, error details, timestamps, execution duration, and an output-truncated flag.

### `shell_list_jobs`

Lists retained jobs, newest first. Optional `limit` (default 25, maximum 100), `offset`, `status`, and case-insensitive `command_contains` filters are supported. The response includes `total`, `limit`, `offset`, and `has_more`.

### `shell_cancel_job`

Cancels a queued or running job. On Unix, command timeouts terminate the command process group; cancellation terminates both command and worker process groups, escalating from TERM to KILL after one second. On Windows, it terminates the direct command and worker processes; descendant-process cleanup is not guaranteed.

### `shell_delete_job`

Deletes a completed job's metadata and stored output. Active jobs must be cancelled first. Use this to reclaim space and keep the retained-job limit available.

Background jobs run in detached worker processes and persist their metadata/output outside the MCP server process. They can continue across an MCP server restart while the host remains running, but are not guaranteed to survive host reboot or power loss. Defaults are 24 hours maximum background runtime, 32 active jobs, and 1,000 retained records. Job data is stored under the user's cache directory in `mcp-shell/jobs`; set `MCP_SHELL_JOB_DIR` to override it. Output is capped per stream by `max_output_size`.

`shell_exec` waits synchronously for up to 20 seconds by default (or the configured `background_after_seconds` threshold). If the command is still running, it returns the durable job ID and current output; use `shell_get_job` to continue retrieving output. Jobs that finish before handoff are removed after their result is collected, so only handed-off jobs consume retained-job capacity. Use `shell_run_background` when you want the job ID immediately.

### `list_allowed_commands`

Returns the effective security mode and executable allowlist loaded by the server. It reflects the runtime configuration after defaults and `MCP_SHELL_SEC_CONFIG_FILE` have been applied; it does not independently parse the YAML file.

### `request_privileged_operation`

Creates a unique, independent one-shot request for a privileged operation. Required parameter: `command` (one direct executable invocation with literal arguments); optional `reason`. Shell operators and shell/language interpreters are rejected. Returns a `request_id` that must be passed to `execute_privileged_operation`.

**Important:** this initial implementation intentionally has no human approval step. Anyone able to call the tools can request and execute commands with the service user's sudo privileges. Use only in a trusted environment and configure sudo deliberately. Execution reads `MCP_SHELL_SUDO_PASSWORD` from the service environment and supplies it to `sudo -S` over stdin; the password is not included in command arguments, request records, or tool output. If the variable is missing, execution fails without consuming the pending request. Configure it through a service-scoped, owner-only systemd `EnvironmentFile`; do not commit it or put it in the repository `.env`. Do not configure unrestricted `NOPASSWD: ALL` unless you deliberately accept unrestricted root access for this MCP server.

### `execute_privileged_operation`

Takes only `request_id`; it does not accept a command. Each request is stored separately and atomically claimed before execution, so concurrent callers cannot overwrite one another or execute the same request twice. Commands execute directly without a shell. Results are recorded in the request state directory.

`MCP_SHELL_PRIVILEGED_STATE_DIR` selects the state directory (default: `$XDG_STATE_HOME/mcp-shell/privileged-requests`, or `~/.local/state/mcp-shell/privileged-requests`). It is created with owner-only permissions. This structure is intended to support a future approval service, such as a WhatsApp accept/reject flow, without changing the per-request identity and one-shot execution model.

---

## Environment variables

| Variable | Description |
|----------|-------------|
| `MCP_SHELL_SEC_CONFIG_FILE` | Path to security YAML (overrides built-in secure defaults) |
| `MCP_SHELL_PRIVILEGED_STATE_DIR` | Directory for independent one-shot privileged-operation request records |
| `MCP_SHELL_JOB_DIR` | Directory for background job metadata and stdout/stderr output |
| `MCP_SHELL_SUDO_PASSWORD` | Sudo password captured at startup and removed from the process environment; required to execute privileged operations |
| `MCP_SHELL_ALLOW_UNSAFE` | Set `true` to disable all validation and run unrestricted (opt-in) |
| `MCP_SHELL_SERVER_NAME` | Server name (default: "mcp-shell 🐚") |
| `MCP_SHELL_LOG_LEVEL` | debug, info, warn, error, fatal |
| `MCP_SHELL_LOG_FORMAT` | json, console |
| `MCP_SHELL_LOG_OUTPUT` | stdout, stderr, file |

---

## Development

```bash
make install dev-tools   # deps + goimports, golines
make fmt test lint
make docker-build       # build image locally
make release            # binary + docker image
```

---

## Security

- **Default**: Secure mode, restricted to a narrow allowlist of read-only utilities. No interpreters.
- **Secure mode** (`use_shell_execution: false`): the command is parsed into a shell AST and only a single, fully-literal simple command is accepted (no pipes, lists, substitution, redirection or globs); its executable must be on the allowlist. Interpreters (bash/sh/python) are hard-denied even if allowlisted, and per-tool policies are deny-by-default: for governed binaries (`git`, `find`, `sort`, `tar`) only explicitly safe flags are accepted and everything else, including unknown or future escape-hatch flags, is rejected (`git -c`/`config`, `find -exec`/`-fls`, `sort -o`/`--compress-program`, `tar -I`/`-C`). Git is limited to read-only subcommands. This is an early-reject layer, not a sandbox.
- **Unrestricted**: Only via `MCP_SHELL_ALLOW_UNSAFE=true`. Full access; fine for local dev, dangerous otherwise.
- **Docker**: Runs as non-root, Alpine-based. Use it in production. Best paired with an OS sandbox (read-only FS, dropped caps) as defense-in-depth.

---

## Contributing

Fork, branch, `make fmt test`, open a PR.
