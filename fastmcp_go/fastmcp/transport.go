package fastmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// JSON-RPC 2.0 error codes used by MCP.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// Serve runs the MCP protocol over newline-delimited JSON on r / w (the
// stdio transport framing). It returns when r reaches EOF or ctx is done.
func (m *FastMCP) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var wmu sync.Mutex
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, err = fmt.Fprintf(w, "%s\n", b)
		return err
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = write(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: codeParseError, Message: "parse error: " + err.Error()}})
			continue
		}

		// Notifications carry no id and get no response.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			m.handleNotification(req)
			continue
		}

		result, rpcErr := m.handleRequest(ctx, req)
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := write(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (m *FastMCP) handleNotification(req request) {
	// notifications/initialized, notifications/cancelled, etc. The MVP has no
	// state to update on these, so they are accepted and ignored.
}

func (m *FastMCP) handleRequest(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return m.initializeResult(), nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return m.listToolsResult(), nil

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call requires a name"}
		}
		if _, ok := m.GetTool(params.Name); !ok {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + params.Name}
		}
		res, err := m.CallTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return nil, &rpcError{Code: codeInternalError, Message: err.Error()}
		}
		return res, nil

	// Declared-but-empty capabilities so clients that probe everything
	// don't see method-not-found errors.
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil

	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

func (m *FastMCP) initializeResult() map[string]any {
	res := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    m.name,
			"version": Version,
		},
	}
	if m.instructions != "" {
		res["instructions"] = m.instructions
	}
	return res
}

type toolListing struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
}

func (m *FastMCP) listToolsResult() map[string]any {
	tools := m.Tools()
	out := make([]toolListing, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolListing{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
		})
	}
	return map[string]any{"tools": out}
}
