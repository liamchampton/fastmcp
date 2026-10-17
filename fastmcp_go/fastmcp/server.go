// Package fastmcp is a minimal Go counterpart to the Python FastMCP framework.
//
// A server wraps ordinary Go functions as MCP tools. Register a function with
// [FastMCP.Tool], then call [FastMCP.Run] to serve it over stdio:
//
//	type AddArgs struct {
//	    A int `json:"a" description:"First number"`
//	    B int `json:"b" description:"Second number"`
//	}
//
//	func add(args AddArgs) int { return args.A + args.B }
//
//	func main() {
//	    mcp := fastmcp.New("Demo")
//	    mcp.Tool(add, fastmcp.WithDescription("Add two numbers"))
//	    mcp.Run()
//	}
package fastmcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
)

// ProtocolVersion is the MCP protocol revision this server speaks.
const ProtocolVersion = "2025-06-18"

// Version is the version reported in the initialize handshake.
const Version = "0.1.0"

// FastMCP is an MCP server that exposes Go functions as tools.
type FastMCP struct {
	name         string
	instructions string

	mu    sync.RWMutex
	tools map[string]*Tool
}

// Option configures a FastMCP server.
type Option func(*FastMCP)

// WithInstructions sets the instructions string sent to clients on initialize.
func WithInstructions(s string) Option {
	return func(m *FastMCP) { m.instructions = s }
}

// New creates a server with the given display name.
func New(name string, opts ...Option) *FastMCP {
	m := &FastMCP{name: name, tools: map[string]*Tool{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Name returns the server's display name.
func (m *FastMCP) Name() string { return m.name }

// Tool registers fn as an MCP tool and returns the resulting Tool.
//
// fn must have one of these shapes, where Args is a struct and R is any
// JSON-serialisable type:
//
//	func(Args) R
//	func(Args) (R, error)
//	func(Args) error
//	func(context.Context, Args) R
//	func(context.Context, Args) (R, error)
//	func(context.Context, Args) error
//	func() R                          (and the ctx / error variants above)
//
// The name defaults to the function name and the schema comes from the Args
// struct's tags. Tool panics on an unsupported signature.
func (m *FastMCP) Tool(fn any, opts ...ToolOption) *Tool {
	t, err := newTool(fn, opts...)
	if err != nil {
		panic(fmt.Sprintf("fastmcp: cannot register tool: %v", err))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.tools[t.Name]; dup {
		panic(fmt.Sprintf("fastmcp: tool %q is already registered", t.Name))
	}
	m.tools[t.Name] = t
	return t
}

// Tools returns all registered tools sorted by name.
func (m *FastMCP) Tools() []*Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tool, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// GetTool looks up a tool by name.
func (m *FastMCP) GetTool(name string) (*Tool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tools[name]
	return t, ok
}

// CallTool invokes a registered tool with JSON-encoded arguments.
func (m *FastMCP) CallTool(ctx context.Context, name string, args []byte) (*ToolResult, error) {
	t, ok := m.GetTool(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	return t.call(ctx, args)
}

// Run serves the server over stdio and exits the process on a transport error.
func (m *FastMCP) Run() {
	if err := m.RunStdio(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "fastmcp:", err)
		os.Exit(1)
	}
}

// RunStdio serves the server over stdin and stdout until stdin closes or ctx
// is cancelled. While serving, os.Stdout points at stderr so a stray
// fmt.Print in a tool can't corrupt the protocol stream.
func (m *FastMCP) RunStdio(ctx context.Context) error {
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = protocolOut }()
	return m.Serve(ctx, os.Stdin, protocolOut)
}
