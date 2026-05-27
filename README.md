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

### Claude Code

```sh
claude mcp add colab-mcp -s user -- \
  colab-mcp-go \
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
args = ["--connect-timeout", "300s"]
tool_timeout_sec = 360
```

`tool_timeout_sec` must exceed `--connect-timeout`; otherwise Codex cancels
`open_colab_browser_connection` while the bridge is still waiting for the
Colab tab to attach. The default of `60` is too short.

`startup_timeout_sec` is usually not needed because the Go binary starts
quickly. Add it only if your Codex client reports MCP server startup timeouts.

### Generic stdio MCP clients (Claude Desktop, Cursor, Cline, etc.)

```json
{
  "mcpServers": {
    "colab-mcp": {
      "command": "colab-mcp-go",
      "args": ["--connect-timeout", "300s"]
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
```

Flags:

- `--log <dir>`: write log files to this directory. If unset, a temporary
  `colab-mcp-go-logs-*` directory is created.
- `--host <host>`: WebSocket bind host. Default: `localhost`.
- `--connect-timeout <duration>`: how long
  `open_colab_browser_connection` waits for the Colab UI. Default: `60s`.
- `--no-browser`: do not open the browser, useful for tests and headless runs.
- `--enable-proxy`: accepted for compatibility and enabled by default.
- `--version`: print version and exit.

## User Flow

Initially, the local MCP server exposes these bridge tools:

- `open_colab_browser_connection`
- `list_colab_tools`
- `call_colab_tool`

`open_colab_browser_connection` opens:

```text
https://colab.research.google.com/notebooks/empty.ipynb#mcpProxyToken=<token>&mcpProxyPort=<port>
```

After the Colab browser session connects over WebSocket, the bridge initializes a
remote MCP client session over that WebSocket and dynamically registers the
remote notebook tools on the local server. When the browser session disconnects,
remote tools are removed and MCP clients receive
`notifications/tools/list_changed`.

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
