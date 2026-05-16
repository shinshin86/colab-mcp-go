# colab-mcp-go

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

## Build

```sh
go build ./cmd/colab-mcp-go
```

## MCP Client Config

```json
{
  "mcpServers": {
    "colab-mcp": {
      "command": "/absolute/path/to/colab-mcp-go",
      "args": [],
      "timeout": 30000
    }
  }
}
```

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
