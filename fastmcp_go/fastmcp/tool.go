package fastmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
)

var (
	ctxType        = reflect.TypeOf((*context.Context)(nil)).Elem()
	errorType      = reflect.TypeOf((*error)(nil)).Elem()
	toolResultType = reflect.TypeOf(ToolResult{})
)

// Tool is a registered MCP tool backed by a Go function.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// OutputSchema describes the tool's structuredContent. It is nil when the
	// return type is not known statically (an interface, a *ToolResult, or no
	// return value at all).
	OutputSchema map[string]any

	fn         reflect.Value
	takesCtx   bool
	argsType   reflect.Type // nil when the function takes no args struct
	returns    returnShape
	wrapResult bool // structuredContent is {"result": value}
}

type returnShape int

const (
	returnsNothing  returnShape = iota // func(...)
	returnsValue                       // func(...) R
	returnsError                       // func(...) error
	returnsValueErr                    // func(...) (R, error)
)

// ToolOption customises a tool at registration time.
type ToolOption func(*Tool)

// WithName overrides the tool name (defaults to the Go function name).
func WithName(name string) ToolOption {
	return func(t *Tool) { t.Name = name }
}

// WithDescription sets the description shown to the model.
func WithDescription(desc string) ToolOption {
	return func(t *Tool) { t.Description = desc }
}

// Content is one block of a tool result, as defined by MCP.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolResult is the payload returned from tools/call.
type ToolResult struct {
	Content           []Content      `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// ToolError is returned by a tool to report a failure to the model without
// treating it as a protocol error. Any other error returned by a tool is
// reported the same way; this type just makes the intent explicit.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

// NewToolError creates a ToolError with a formatted message.
func NewToolError(format string, a ...any) error {
	return &ToolError{Msg: fmt.Sprintf(format, a...)}
}

func newTool(fn any, opts ...ToolOption) (*Tool, error) {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		return nil, errors.New("expected a function")
	}
	ft := v.Type()
	if ft.IsVariadic() {
		return nil, errors.New("variadic functions are not supported")
	}

	t := &Tool{fn: v, Name: funcName(fn)}

	// Inputs: optional context.Context, then an optional args struct.
	in := make([]reflect.Type, 0, ft.NumIn())
	for i := 0; i < ft.NumIn(); i++ {
		in = append(in, ft.In(i))
	}
	if len(in) > 0 && in[0] == ctxType {
		t.takesCtx = true
		in = in[1:]
	}
	switch len(in) {
	case 0:
	case 1:
		if in[0].Kind() != reflect.Struct {
			return nil, fmt.Errorf("argument must be a struct, got %s (Go reflection cannot see parameter names, so wrap inputs in a struct)", in[0])
		}
		t.argsType = in[0]
	default:
		return nil, fmt.Errorf("too many arguments: expected (ctx, args) at most, got %d", ft.NumIn())
	}

	// Outputs.
	switch ft.NumOut() {
	case 0:
		t.returns = returnsNothing
	case 1:
		if ft.Out(0) == errorType {
			t.returns = returnsError
		} else {
			t.returns = returnsValue
		}
	case 2:
		if ft.Out(1) != errorType {
			return nil, errors.New("second return value must be error")
		}
		t.returns = returnsValueErr
	default:
		return nil, errors.New("too many return values")
	}

	t.InputSchema = schemaForArgs(t.argsType)
	if t.returns == returnsValue || t.returns == returnsValueErr {
		t.OutputSchema, t.wrapResult = outputSchemaFor(ft.Out(0))
	}

	for _, o := range opts {
		o(t)
	}
	if t.Name == "" {
		return nil, errors.New("tool has no name; use WithName for anonymous functions")
	}
	return t, nil
}

// funcName extracts the bare function name ("add") from a function value.
// Anonymous functions and methods yield "" and must use WithName.
func funcName(fn any) string {
	pc := reflect.ValueOf(fn).Pointer()
	rf := runtime.FuncForPC(pc)
	if rf == nil {
		return ""
	}
	name := rf.Name() // e.g. "main.add" or "main.main.func1" or "pkg.(*T).Method-fm"
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if strings.HasPrefix(name, "func") || strings.HasSuffix(name, "-fm") {
		return ""
	}
	return name
}

// call decodes args into the tool's args struct, invokes the function, and
// converts the result into an MCP ToolResult.
func (t *Tool) call(ctx context.Context, rawArgs []byte) (result *ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = errorResult(fmt.Sprintf("tool %s panicked: %v", t.Name, r))
			err = nil
		}
	}()

	callArgs := make([]reflect.Value, 0, 2)
	if t.takesCtx {
		callArgs = append(callArgs, reflect.ValueOf(ctx))
	}
	if t.argsType != nil {
		argsPtr := reflect.New(t.argsType)
		if len(rawArgs) > 0 && string(rawArgs) != "null" {
			dec := json.NewDecoder(strings.NewReader(string(rawArgs)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(argsPtr.Interface()); err != nil {
				return errorResult(fmt.Sprintf("invalid arguments for %s: %v", t.Name, err)), nil
			}
		}
		if missing := missingRequired(t.InputSchema, rawArgs); len(missing) > 0 {
			return errorResult(fmt.Sprintf("invalid arguments for %s: missing required field(s): %s", t.Name, strings.Join(missing, ", "))), nil
		}
		callArgs = append(callArgs, argsPtr.Elem())
	}

	out := t.fn.Call(callArgs)

	var value reflect.Value
	var callErr error
	switch t.returns {
	case returnsNothing:
	case returnsValue:
		value = out[0]
	case returnsError:
		callErr, _ = out[0].Interface().(error)
	case returnsValueErr:
		value = out[0]
		callErr, _ = out[1].Interface().(error)
	}
	if callErr != nil {
		return errorResult(callErr.Error()), nil
	}
	if !value.IsValid() {
		return &ToolResult{Content: []Content{}}, nil
	}
	return t.valueResult(value)
}

func errorResult(msg string) *ToolResult {
	return &ToolResult{Content: []Content{{Type: "text", Text: msg}}, IsError: true}
}

// outputSchemaFor builds the outputSchema for a return type, following the
// Python framework: MCP requires output schemas to be objects, so any other
// type is wrapped as {"result": ...} and marked with x-fastmcp-wrap-result,
// which tells FastMCP clients to unwrap it again. Pointers are nullable and
// so are wrapped too, like Optional[Model] in Python.
//
// Interfaces and ToolResult have no static shape, so they get no schema.
func outputSchemaFor(rt reflect.Type) (schema map[string]any, wrap bool) {
	if rt.Kind() == reflect.Interface || rt == toolResultType ||
		(rt.Kind() == reflect.Pointer && rt.Elem() == toolResultType) {
		return nil, false
	}
	// The top-level value is never encoded as null: valueResult turns a nil
	// slice or map into [] or {}, and a nil pointer is handled just below.
	b := newSchemaBuilder(outputSchema)
	if rt.Kind() != reflect.Pointer {
		if s := b.root(rt); s["type"] == "object" {
			return s, false
		}
		b = newSchemaBuilder(outputSchema)
	}
	result := b.typeSchema(rt)
	if rt.Kind() == reflect.Pointer {
		result = map[string]any{"anyOf": []any{result, map[string]any{"type": "null"}}}
	}
	return b.withDefs(map[string]any{
		"type":                  "object",
		"properties":            map[string]any{"result": result},
		"required":              []string{"result"},
		"x-fastmcp-wrap-result": true,
	}), true
}

// valueResult mirrors FastMCP's return-value conversion: strings are sent
// verbatim as text, everything else is JSON-encoded. The value is also attached
// as structuredContent, shaped to match the tool's OutputSchema, so clients
// that understand it get typed data.
func (t *Tool) valueResult(rv reflect.Value) (*ToolResult, error) {
	// A nil slice or map is still a list or object to the caller; encode it
	// as [] or {} rather than null so it matches the declared schema.
	if (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.IsNil() {
		if rv.Kind() == reflect.Slice {
			rv = reflect.MakeSlice(rv.Type(), 0, 0)
		} else {
			rv = reflect.MakeMap(rv.Type())
		}
	}
	v := rv.Interface()

	switch x := v.(type) {
	case nil:
		return &ToolResult{Content: []Content{}}, nil
	case *ToolResult:
		return x, nil
	case ToolResult:
		return &x, nil
	case error:
		return errorResult(x.Error()), nil
	}

	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("cannot serialise tool result: %w", err)
		}
		text = string(b)
	}
	res := &ToolResult{Content: []Content{{Type: "text", Text: text}}}

	wrap := t.wrapResult
	if t.OutputSchema == nil {
		// No static schema (the function returns an interface), so decide
		// from the dynamic value: structuredContent must be a JSON object.
		k := reflect.Indirect(reflect.ValueOf(v)).Kind()
		wrap = k != reflect.Struct && k != reflect.Map
	}
	if wrap {
		res.StructuredContent = map[string]any{"result": v}
		// Same marker the Python server sends; FastMCP clients check it to
		// unwrap even when the tool has no outputSchema.
		res.Meta = map[string]any{"fastmcp": map[string]any{"wrap_result": true}}
	} else {
		res.StructuredContent = v
	}
	return res, nil
}

// missingRequired reports required schema properties absent from rawArgs.
func missingRequired(schema map[string]any, rawArgs []byte) []string {
	req, _ := schema["required"].([]string)
	if len(req) == 0 {
		return nil
	}
	var present map[string]json.RawMessage
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &present)
	}
	var missing []string
	for _, k := range req {
		if _, ok := present[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}
