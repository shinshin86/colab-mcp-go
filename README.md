# colab-mcp-go

<p align="center">
  <img src="assets/logo.jpg" alt="colab-mcp-go logo" width="640">
</p>

`colab-mcp-go` is a Go port of the local bridge from
[`googlecolab/colab-mcp`](https://github.com/googlecolab/colab-mcp). It runs as
a single local MCP server binary and does not require Python, `uv`, FastMCP,
`mcp[cli]`, Pydantic, or Python `websockets` at runtime.

The binary is started by an MCP client over stdio. It starts a local WebSocket
server for a Google Colab browser session, then proxies tools exposed by the
Colab-side MCP server back into the local MCP client. Stdout is reserved for MCP
JSON-RPC messages only; operational logs are written to a log file.

The current upstream bridge and Colab UI path use MCP tools and
`notifications/tools/list_changed`; prompts/resources are not exposed by the
checked Python bridge source.

## Install

```sh
go install github.com/shinshin86/colab-mcp-go/cmd/colab-mcp-go@latest
```

Requires Go 1.25+. The binary is placed in `$(go env GOPATH)/bin`. Make sure
that directory is on `PATH`; if your MCP client cannot find the binary, use the
absolute path instead.

To build from a local checkout instead:

```sh
go build ./cmd/colab-mcp-go
```

## Quickstart

`open_colab_browser_connection` blocks until you have opened the Colab tab in
your browser. The examples below give it up to five minutes, which is usually
enough for a fresh Google sign-in. Tune `--connect-timeout` (and the matching
client-side timeout) to taste.

By default, each server start uses an ephemeral WebSocket port and a new
connection token. To reconnect the same browser tab after an MCP client or
server restart, configure both a stable port and a persistent token file:

```sh
colab-mcp-go \
  --port 8765 \
  --token-file ~/.config/colab-mcp-go/connection-token \
  --connect-timeout 300s
```

The token file is created with owner-only permissions and its value is never
written to the server log. Do not commit or share the token file; to rotate
it, stop the bridge and delete the file before the next start.

When `--token-file` is set, the bridge also maintains a non-secret `state.json`
beside it and holds an OS-backed single-instance lock for that token directory.
The state records the PID, bound port, start time, browser connection state, and
last update time. It is removed after a normal shutdown; the lock is released
automatically by the OS if the process exits unexpectedly. Log files default to
a `logs` directory beside the token file unless `--log` is given.

MCP clients often start one bridge process per session, window, or sub-agent,
so several bridges may share the same configuration at the same time. Only the
first process can own the fixed port and the state lock. Later processes do not
exit: they continue on an ephemeral port without the shared lock, so each MCP
client still gets a working `open_colab_browser_connection`. Such a process
reports `instance_mode: "fallback"` and a `fallback_reason` from
`get_colab_connection_status`, and its Colab tab reconnects only while that
process is alive. Pass `--no-fallback` to restore the previous behaviour of
exiting with an error instead.

### Claude Code

```sh
claude mcp add colab-mcp -s user -- \
  colab-mcp-go \
  --port 8765 \
  --token-file ~/.config/colab-mcp-go/connection-token \
  --connect-timeout 300s
```

Use `-s local` to scope the server to the current project instead of all
projects. After running `claude mcp add`, restart Claude Code so the new
session picks up the server; then call the `open_colab_browser_connection`
tool.

### Codex CLI

Add to `~/.codex/config.toml`:

```toml
[mcp_servers.colab-mcp]
command = "colab-mcp-go"
args = [
  "--port", "8765",
  "--token-file", "~/.config/colab-mcp-go/connection-token",
  "--connect-timeout", "300s",
]
tool_timeout_sec = 360
```

`tool_timeout_sec` must exceed `--connect-timeout`; otherwise Codex cancels
`open_colab_browser_connection` while the bridge is still waiting for the
Colab tab to attach. The default of `60` is too short.

`startup_timeout_sec` is usually not needed because the Go binary starts
quickly. Add it only if your Codex client reports MCP server startup timeouts.

Codex starts a separate bridge process for every thread that uses this
configuration, including sub-agents and additional windows. With the fallback
behaviour described in Quickstart, every thread receives working tools; the
first process keeps port `8765` and the state lock, and later ones use an
ephemeral port. If a thread reports that the Colab tools are missing, check the
bridge log directory (`~/.config/colab-mcp-go/logs` with the settings above)
and `colab-mcp-go doctor`.

### Generic stdio MCP clients (Claude Desktop, Cursor, Cline, etc.)

```json
{
  "mcpServers": {
    "colab-mcp": {
      "command": "colab-mcp-go",
      "args": [
        "--port", "8765",
        "--token-file", "~/.config/colab-mcp-go/connection-token",
        "--connect-timeout", "300s"
      ]
    }
  }
}
```

If your client also exposes a per-tool or initialization timeout (often in
milliseconds), set it above `--connect-timeout` — for example `360000` ms —
so the open-connection call is not cancelled prematurely.

## CLI

```sh
colab-mcp-go [flags]
colab-mcp-go doctor [flags]
```

Flags:

- `--log <dir>`: write log files to this directory. If unset and
  `--token-file` is set, a `logs` directory beside the token file is used;
  otherwise a temporary `colab-mcp-go-logs-*` directory is created.
- `--host <host>`: WebSocket bind host. Default: `localhost`.
- `--port <port>`: WebSocket bind port. Default: `0`, which chooses an
  ephemeral port. Set a stable port together with `--token-file` to reconnect
  after a server restart.
- `--token-file <path>`: read or create a persistent URL-safe connection token.
  New files use owner-only permissions. `~` is expanded to the current user's
  home directory.
- `--connect-timeout <duration>`: how long
  `open_colab_browser_connection` waits for the Colab UI. Default: `60s`.
- `--no-browser`: do not open the browser, useful for tests and headless runs.
- `--no-fallback`: exit with an error when the configured port or the
  single-instance lock is unavailable, instead of continuing on an ephemeral
  port (see Quickstart).
- `--enable-proxy`: accepted for compatibility and enabled by default.
- `--version`: print version and exit.

`doctor` performs read-only local diagnostics and never kills, restarts, or
repairs a process. Pass the same connection settings used by the bridge:

```sh
colab-mcp-go doctor \
  --host localhost \
  --port 8765 \
  --token-file ~/.config/colab-mcp-go/connection-token \
  --log ~/.config/colab-mcp-go/logs
```

It checks the listening port and owner when available, verifies `/healthz`,
compares `state.json` with the live PID and port, validates token-file type and
permissions, and recognizes common errors near the end of a supplied log file
or directory. When `--log` is omitted and `--token-file` is set, the `logs`
directory beside the token file is inspected if it exists. Add `--json` for machine-readable output. A healthy report exits
with status 0; warnings and errors exit with status 1; invalid CLI usage exits
with status 2.

The local WebSocket HTTP server exposes unauthenticated `GET /healthz` for
identity and liveness checks. Its response contains only the bridge name,
version, PID, browser connection state, and start time. WebSocket origin and
token checks remain unchanged.

## User Flow

Initially, the local MCP server exposes these bridge tools:

- `open_colab_browser_connection`
- `list_colab_tools`
- `call_colab_tool`
- `get_colab_connection_status`
- `disconnect_colab_runtime`

With no arguments, `open_colab_browser_connection` opens:

```text
https://colab.research.google.com/notebooks/empty.ipynb#mcpProxyToken=<token>&mcpProxyPort=<port>
```

To connect an existing notebook instead, pass its URL:

```json
{
  "notebook_url": "https://colab.research.google.com/drive/<notebook-id>"
}
```

Only HTTPS URLs on `colab.research.google.com` and `colab.google.com` are
accepted. The bridge replaces any existing URL fragment with its connection
parameters. With a stable `--port` and `--token-file`, the same notebook URL can
be opened again after a server restart; reload the notebook tab if it does not
reconnect automatically.

The bridge accepts one browser connection at a time. Close the current Colab
browser connection before using `notebook_url` to switch to another notebook.

If the configured port or the state lock is already taken, another bridge
process is still running (for example one started by a different MCP client
session). The new process then continues on an ephemeral port and reports
`instance_mode: "fallback"`; pass `--no-fallback` to make it exit instead.

After the Colab browser session connects over WebSocket, the bridge initializes a
remote MCP client session over that WebSocket and dynamically registers the
remote notebook tools on the local server. When the browser session disconnects,
remote tools are removed and MCP clients receive
`notifications/tools/list_changed`.

`get_colab_connection_status` reads local bridge state only. It reports the
PID, WebSocket port, browser and remote-session connection state, remote tool
count, uptime, version, `instance_mode` (`primary` or `fallback`),
`configured_port`, and `fallback_reason` without making a Colab call.

`disconnect_colab_runtime` disconnects and deletes the currently assigned Colab
runtime (the same effect as "Runtime > Disconnect and delete runtime") by
executing `google.colab.runtime.unassign()` in the kernel via the notebook's
cell tools. This stops compute-unit consumption for GPU runtimes. Notes:

- Destructive: in-memory state and files outside mounted Drive are lost.
- The result reports `outcome: completed | unknown`; failures before the
  unassign cell is issued are reported as tool errors. The final run step
  often reports an error or timeout because the kernel terminates while
  executing it; that is reported as `unknown` and can still mean the
  disconnect succeeded. Verify in the Colab UI, and do not run additional
  cells to check — running a cell can assign a fresh runtime.
- When the cell-id runner path is used (current Colab builds), a marker
  comment cell is added to the notebook and remains afterwards as a record of
  the disconnect. Re-running that cell later disconnects whatever runtime is
  assigned at that time. Runners that take code directly leave no cell behind.
- The browser tab and the bridge connection stay alive; only the runtime is
  released. Reconnecting a runtime from the Colab UI resumes normal use.

## Development

```sh
go test ./...
go vet ./...
go test -race ./...
```

## Attribution

This project is a Go port of
[`googlecolab/colab-mcp`](https://github.com/googlecolab/colab-mcp).

Portions of this project are based on `googlecolab/colab-mcp` and are used
under the Apache License, Version 2.0.

This project is not an official Google product.

## License

This project is licensed under the Apache License, Version 2.0. See the
LICENSE file for details.

`googlecolab/colab-mcp` is licensed under the Apache License, Version 2.0.
