# FastMCP for Go (MVP)

A minimal Go counterpart to the Python FastMCP server. It wraps ordinary Go
functions as MCP tools and serves them over stdio, using only the standard
library.

```go
package main

import "github.com/PrefectHQ/fastmcp/fastmcp_go/fastmcp"

type AddArgs struct {
    A int `json:"a" description:"First number"`
    B int `json:"b" description:"Second number"`
}

func add(args AddArgs) int { return args.A + args.B }

func main() {
    mcp := fastmcp.New("Demo 🚀")
    mcp.Tool(add, fastmcp.WithDescription("Add two numbers"))
    mcp.Run()
}
```

That is the Go version of the Python README example:

```python
mcp = FastMCP("Demo 🚀")

@mcp.tool
def add(a: int, b: int) -> int:
    """Add two numbers"""
    return a + b

mcp.run()
```

## Run the demos

The demo server in [`examples/demo`](examples/demo/main.go) has three tools:

| Tool      | Input                 | Returns                                          |
| --------- | --------------------- | ------------------------------------------------ |
| `add`     | `a`, `b` (integers)   | Their sum                                        |
| `greet`   | `name`, `shout`       | A greeting (and logs a line to stderr)           |
| `weather` | `city`                | `{city, temp_c, conditions}`, or an error if the city is unknown |

The weather data is made up and covers London, Paris and Tokyo, so no API key
or network is needed.

**You need:** Go 1.22 or newer. Demo 4 also needs Node.js (for `npx`), and
demo 5 needs [uv](https://docs.astral.sh/uv/) to run the Python client from
this repository.

Run every command below from the `fastmcp_go` directory:

```bash
cd fastmcp_go
```

### 1. Run the tests

```bash
go test ./...
```

You should see `ok` for the `fastmcp` package.

### 2. Build the demo server

The other demos use this binary:

```bash
go build -o /tmp/demo-go ./examples/demo
```

### 3. Talk to it by hand

An MCP client and server exchange one JSON-RPC message per line over stdin and
stdout. This sends the handshake, lists the tools, and calls each one:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cli","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add","arguments":{"a":40,"b":2}}}' \
  '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"greet","arguments":{"name":"Ada"}}}' \
  '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"weather","arguments":{"city":"Paris"}}}' \
  | /tmp/demo-go
```

Each reply is a line of JSON. Look for `"text":"42"`, `"text":"Hello, Ada!"`
and `"temp_c":18`. The line `greet called for Ada` is the tool's own
`fmt.Println`: it goes to stderr, so it can't corrupt the replies.

### 4. Explore it in MCP Inspector

[MCP Inspector](https://github.com/modelcontextprotocol/inspector) is a
browser UI for any MCP server:

```bash
npx @modelcontextprotocol/inspector /tmp/demo-go
```

It prints a URL that includes a login token and opens it in your browser. If
the browser doesn't open, copy the full URL from the terminal, token included.

These steps match Inspector v2.11:

1. On the `demo-go` server card, turn on the toggle. The status changes to
   **Connected**.
2. Click **Tools** in the top bar. The three tools are listed on the left.
3. Click `weather`. The form on the right is built from the Go struct tags,
   so it shows the tool description and a required `city` field with the hint
   "City name, e.g. London".
4. Type `London` and click **Execute Tool**. The result appears under
   **Results** and **Structured Output**.
5. Try `Atlantis` to see the error, shown as **Tool Error**.

Press Ctrl-C in the terminal to stop Inspector.

### 5. Call it from the Python FastMCP client

The Python client in this repository connects to the Go server and gets the
same results it would from a Python server, including unwrapped values such as
`42`:

```bash
uv run python - <<'EOF'
import asyncio
from fastmcp import Client
from fastmcp.client.transports import StdioTransport

async def main():
    transport = StdioTransport(command="/tmp/demo-go", args=[])
    async with Client(transport) as client:
        tools = await client.list_tools()
        print([tool.name for tool in tools])
        print((await client.call_tool("add", {"a": 40, "b": 2})).data)
        weather = (await client.call_tool("weather", {"city": "Tokyo"})).data
        print(weather.city, weather.temp_c, weather.conditions)

asyncio.run(main())
EOF
```

Expected output:

```
['add', 'greet', 'weather']
42
Tokyo 22 Rain
```

The `weather` result arrives as a typed object built from the tool's output
schema, which is why `weather.temp_c` works.

### 6. Use it from Claude

Register the binary as an MCP server in Claude Code:

```bash
claude mcp add demo-go -- /tmp/demo-go
```

Then ask Claude "What's the weather in Paris?" and it calls the Go `weather`
tool. Remove it afterwards with `claude mcp remove demo-go`.

For Claude Desktop, add the server to `claude_desktop_config.json` and restart
the app:

```json
{
  "mcpServers": {
    "demo-go": { "command": "/tmp/demo-go" }
  }
}
```

`/tmp` is cleared when your machine restarts, so rebuild the binary (demo 2)
or build it somewhere permanent if you want the server to stay registered.

## How it maps to the Python framework

| Python                                   | Go                                      |
| ---------------------------------------- | --------------------------------------- |
| `FastMCP("name")`                        | `fastmcp.New("name")`                   |
| `@mcp.tool`                              | `mcp.Tool(fn, ...opts)`                 |
| Docstring                                | `fastmcp.WithDescription("...")`        |
| Parameter names and types                | Fields of the args struct (`json` tags) |
| `Annotated[int, Field(description=...)]` | `description:"..."` struct tag          |
| Optional parameter                       | Pointer field or `json:",omitempty"`    |
| `raise ToolError(...)`                   | `return fastmcp.NewToolError(...)`      |
| `mcp.run()`                              | `mcp.Run()`                             |

Go reflection cannot see parameter names, so inputs live in a struct instead of
being positional parameters. Everything else is derived automatically: the tool
name comes from the function name, the input schema from the args struct, and
the output schema from the return type.

Return values follow the Python framework exactly. Structs and maps are sent as
`structuredContent` directly. Anything else (an `int`, a `string`, a slice, a
pointer) is wrapped as `{"result": ...}` and marked with
`x-fastmcp-wrap-result`, so FastMCP clients unwrap it again and
`client.call_tool("add", ...).data` is `42`, not `{"result": 42}`.

Supported function shapes (`Args` is any struct, `R` any JSON-serialisable type):

```
func(Args) R
func(Args) (R, error)
func(Args) error
func(context.Context, Args) R        // and the error variants
func() R                             // tools with no inputs
```

## What is in the MVP

- `initialize`, `ping`, `tools/list`, `tools/call` over newline-delimited
  JSON on stdio, plus empty `resources/list` and `prompts/list` so probing
  clients get clean answers.
- Reflection-based tool registration with input and output schema generation
  for bools, numbers, strings, slices, maps, nested and self-referencing
  structs (via `$defs`), pointers, `time.Time`, and `description` / `default` /
  `enum` struct tags.
- Output schemas that match what `encoding/json` produces, so strict clients
  accept the results: nil pointers, slices and maps inside a result are
  nullable, and a nil top-level slice or map is sent as `[]` or `{}`.
- Argument validation (unknown fields, wrong types, missing required fields)
  reported as tool errors, and panics inside tools recovered into tool errors.
- Safe printing: stdout carries the protocol, so while the server runs
  `os.Stdout` points at stderr. A `fmt.Println` left in a tool shows up in the
  server log instead of corrupting a response. Writes made directly to file
  descriptor 1, or through a writer that captured `os.Stdout` before `Run()`
  (such as `log.New(os.Stdout, ...)` at package init), are not redirected.

Not yet: resources, prompts, HTTP transport, auth, middleware, or a client.
