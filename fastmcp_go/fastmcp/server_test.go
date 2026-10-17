package fastmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

type addArgs struct {
	A int `json:"a" description:"First number"`
	B int `json:"b"`
}

func add(args addArgs) int { return args.A + args.B }

type greetArgs struct {
	Name  string  `json:"name"`
	Title *string `json:"title"`
	Loud  bool    `json:"loud,omitempty"`
}

func greet(args greetArgs) string {
	s := "Hello, " + args.Name
	if args.Title != nil {
		s = "Hello, " + *args.Title + " " + args.Name
	}
	if args.Loud {
		s = strings.ToUpper(s)
	}
	return s
}

type point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func origin() point { return point{} }

func fail(args addArgs) (int, error) { return 0, errors.New("boom") }

func withCtx(ctx context.Context, args addArgs) (int, error) {
	if ctx == nil {
		return 0, errors.New("nil ctx")
	}
	return args.A, nil
}

func TestToolRegistrationAndSchema(t *testing.T) {
	m := New("test")
	tool := m.Tool(add, WithDescription("Add two numbers"))

	if tool.Name != "add" {
		t.Fatalf("name = %q, want add", tool.Name)
	}
	if tool.Description != "Add two numbers" {
		t.Fatalf("description = %q", tool.Description)
	}
	props := tool.InputSchema["properties"].(map[string]any)
	a := props["a"].(map[string]any)
	if a["type"] != "integer" || a["description"] != "First number" {
		t.Fatalf("unexpected schema for a: %v", a)
	}
	req := tool.InputSchema["required"].([]string)
	if strings.Join(req, ",") != "a,b" {
		t.Fatalf("required = %v", req)
	}
}

func TestOptionalFields(t *testing.T) {
	m := New("test")
	tool := m.Tool(greet)
	req := tool.InputSchema["required"].([]string)
	if strings.Join(req, ",") != "name" {
		t.Fatalf("required = %v, want only name", req)
	}
	props := tool.InputSchema["properties"].(map[string]any)
	if props["title"].(map[string]any)["type"] != "string" {
		t.Fatalf("pointer field should unwrap to string: %v", props["title"])
	}
}

func TestCallTool(t *testing.T) {
	m := New("test")
	m.Tool(add)
	m.Tool(greet)
	m.Tool(origin)
	m.Tool(fail)
	m.Tool(withCtx)
	ctx := context.Background()

	res, err := m.CallTool(ctx, "add", []byte(`{"a": 2, "b": 3}`))
	if err != nil || res.IsError || res.Content[0].Text != "5" {
		t.Fatalf("add: res=%+v err=%v", res, err)
	}
	if sc := res.StructuredContent.(map[string]any)["result"]; sc != 5 {
		t.Fatalf("structuredContent = %v", res.StructuredContent)
	}

	res, _ = m.CallTool(ctx, "greet", []byte(`{"name": "Ada", "loud": true}`))
	if res.Content[0].Text != "HELLO, ADA" {
		t.Fatalf("greet = %q", res.Content[0].Text)
	}
	if sc := res.StructuredContent.(map[string]any)["result"]; sc != "HELLO, ADA" {
		t.Fatalf("string structuredContent = %v", res.StructuredContent)
	}

	res, _ = m.CallTool(ctx, "origin", nil)
	if res.Content[0].Text != `{"x":0,"y":0}` {
		t.Fatalf("origin = %q", res.Content[0].Text)
	}
	if _, ok := res.StructuredContent.(point); !ok {
		t.Fatalf("struct result should be structuredContent as-is, got %T", res.StructuredContent)
	}

	res, _ = m.CallTool(ctx, "fail", []byte(`{"a":1,"b":1}`))
	if !res.IsError || res.Content[0].Text != "boom" {
		t.Fatalf("fail = %+v", res)
	}

	res, _ = m.CallTool(ctx, "withCtx", []byte(`{"a":7,"b":1}`))
	if res.IsError || res.Content[0].Text != "7" {
		t.Fatalf("withCtx = %+v", res)
	}

	// Validation errors are tool errors, not transport errors.
	res, _ = m.CallTool(ctx, "add", []byte(`{"a": 1}`))
	if !res.IsError || !strings.Contains(res.Content[0].Text, "missing required field(s): b") {
		t.Fatalf("missing field: %+v", res)
	}
	res, _ = m.CallTool(ctx, "add", []byte(`{"a": "x", "b": 1}`))
	if !res.IsError {
		t.Fatalf("wrong type should be an error: %+v", res)
	}

	if _, err := m.CallTool(ctx, "nope", nil); err == nil {
		t.Fatal("unknown tool should error")
	}
}

func names() []string              { return nil }
func lookup() *point               { return nil }
func scores() map[string]int       { return nil }
func anything() any                { return 3 }
func noop(args addArgs)            {}
func custom() *ToolResult          { return &ToolResult{Content: []Content{{Type: "text", Text: "hi"}}} }
func wrapped(s map[string]any) any { return s["properties"].(map[string]any)["result"] }

func TestOutputSchema(t *testing.T) {
	m := New("test")
	for _, fn := range []any{add, greet, origin, fail, names, lookup, scores, anything, noop, custom} {
		m.Tool(fn)
	}
	tool := func(name string) *Tool { t.Helper(); tl, _ := m.GetTool(name); return tl }

	// Non-object returns are wrapped and marked so FastMCP clients unwrap them.
	for name, want := range map[string]string{"add": "integer", "greet": "string", "fail": "integer", "names": "array"} {
		s := tool(name).OutputSchema
		if s["type"] != "object" || s["x-fastmcp-wrap-result"] != true {
			t.Fatalf("%s: expected wrapped object schema, got %v", name, s)
		}
		if got := wrapped(s).(map[string]any)["type"]; got != want {
			t.Fatalf("%s: result type = %v, want %s", name, got, want)
		}
	}

	// Struct and map returns are already objects and are not wrapped.
	for _, name := range []string{"origin", "scores"} {
		s := tool(name).OutputSchema
		if s["type"] != "object" || s["x-fastmcp-wrap-result"] != nil {
			t.Fatalf("%s: expected unwrapped object schema, got %v", name, s)
		}
	}
	if props := tool("origin").OutputSchema["properties"].(map[string]any); props["x"] == nil || props["y"] == nil {
		t.Fatalf("origin properties = %v", props)
	}

	// Pointers can be nil, so they are wrapped and nullable.
	ptr := wrapped(tool("lookup").OutputSchema).(map[string]any)["anyOf"].([]any)
	if ptr[0].(map[string]any)["type"] != "object" || ptr[1].(map[string]any)["type"] != "null" {
		t.Fatalf("lookup result schema = %v", ptr)
	}

	// No static shape, no schema.
	for _, name := range []string{"anything", "noop", "custom"} {
		if s := tool(name).OutputSchema; s != nil {
			t.Fatalf("%s: expected no output schema, got %v", name, s)
		}
	}
}

type profile struct {
	Name  string            `json:"name"`
	Title *string           `json:"title"`
	Tags  []string          `json:"tags"`
	Meta  map[string]string `json:"meta"`
	Kind  string            `json:"kind" enum:"a,b" default:"a"`
}

func getProfile() profile { return profile{Name: "Ada"} }

func TestOutputSchemaNilableFields(t *testing.T) {
	m := New("test")
	tool := m.Tool(getProfile)
	props := tool.OutputSchema["properties"].(map[string]any)

	// encoding/json writes nil pointers, slices and maps as null, so the
	// output schema must accept null there or clients reject the result.
	for _, name := range []string{"title", "tags", "meta"} {
		anyOf, ok := props[name].(map[string]any)["anyOf"].([]any)
		if !ok || anyOf[1].(map[string]any)["type"] != "null" {
			t.Fatalf("%s should be nullable in output schema, got %v", name, props[name])
		}
	}
	if props["name"].(map[string]any)["type"] != "string" {
		t.Fatalf("name should stay a plain string, got %v", props["name"])
	}
	// enum and default describe inputs and are left out of output schemas.
	if kind := props["kind"].(map[string]any); kind["enum"] != nil || kind["default"] != nil {
		t.Fatalf("kind should have no enum/default in output schema, got %v", kind)
	}

	// Input schemas are unchanged: pointers there mean "optional", not null.
	in := schemaForArgs(reflect.TypeOf(profile{}))["properties"].(map[string]any)
	if in["title"].(map[string]any)["type"] != "string" || in["kind"].(map[string]any)["enum"] == nil {
		t.Fatalf("input schema changed: %v", in)
	}
}

type node struct {
	Name     string  `json:"name"`
	Children []*node `json:"children" description:"Child nodes"`
}

type treeArgs struct {
	Root node `json:"root"`
}

func tree(args treeArgs) node { return args.Root }

func treeList() []node { return []node{{Name: "a"}} }

func TestRecursiveTypes(t *testing.T) {
	m := New("test")
	// Self-referencing types used to overflow the stack at registration.
	tool := m.Tool(tree)
	m.Tool(treeList)

	defs, ok := tool.InputSchema["$defs"].(map[string]any)
	if !ok || defs["node"] == nil {
		t.Fatalf("input schema should define node under $defs: %v", tool.InputSchema)
	}
	root := tool.InputSchema["properties"].(map[string]any)["root"].(map[string]any)
	if root["allOf"].([]any)[0].(map[string]any)["$ref"] != "#/$defs/node" {
		t.Fatalf("root arg should reference node: %v", root)
	}
	children := defs["node"].(map[string]any)["properties"].(map[string]any)["children"].(map[string]any)
	if children["description"] != "Child nodes" ||
		children["items"].(map[string]any)["$ref"] != "#/$defs/node" {
		t.Fatalf("children should be an array of node refs: %v", children)
	}

	// A recursive struct output is inlined at the root, so it is still an
	// unwrapped object, with $defs for the inner references.
	out := tool.OutputSchema
	if out["type"] != "object" || out["x-fastmcp-wrap-result"] != nil || out["$defs"] == nil {
		t.Fatalf("recursive output schema = %v", out)
	}
	if _, nested := out["$defs"].(map[string]any)["node"].(map[string]any)["$defs"]; nested {
		t.Fatal("$defs should not be nested inside its own definition")
	}

	// Wrapped outputs carry $defs at the wrapper root, where $ref resolves.
	list, _ := m.GetTool("treeList")
	if list.OutputSchema["x-fastmcp-wrap-result"] != true || list.OutputSchema["$defs"] == nil {
		t.Fatalf("wrapped recursive output schema = %v", list.OutputSchema)
	}

	// The schema must also be valid JSON.
	if _, err := json.Marshal(tool.InputSchema); err != nil {
		t.Fatal(err)
	}

	res, err := m.CallTool(context.Background(), "tree",
		[]byte(`{"root": {"name": "a", "children": [{"name": "b", "children": []}]}}`))
	if err != nil || res.IsError || res.StructuredContent.(node).Children[0].Name != "b" {
		t.Fatalf("tree: res=%+v err=%v", res, err)
	}
}

func TestStructuredContentMatchesSchema(t *testing.T) {
	m := New("test")
	for _, fn := range []any{names, lookup, scores, anything, custom} {
		m.Tool(fn)
	}
	ctx := context.Background()
	call := func(name string) *ToolResult {
		t.Helper()
		res, err := m.CallTool(ctx, name, nil)
		if err != nil || res.IsError {
			t.Fatalf("%s: res=%+v err=%v", name, res, err)
		}
		return res
	}

	// A nil slice is still a list: [] not null.
	if res := call("names"); res.Content[0].Text != "[]" ||
		reflect.ValueOf(res.StructuredContent.(map[string]any)["result"]).Len() != 0 {
		t.Fatalf("names = %+v", res)
	}
	// A nil map is still an object: {} not null.
	if res := call("scores"); res.Content[0].Text != "{}" {
		t.Fatalf("scores = %+v", res)
	}
	// A nil pointer is wrapped as {"result": null}.
	if sc := call("lookup").StructuredContent.(map[string]any); sc["result"] != (*point)(nil) {
		t.Fatalf("lookup structuredContent = %v", sc)
	}
	// An interface return falls back to deciding from the dynamic value, and
	// marks the wrap in _meta so clients can unwrap without a schema.
	res := call("anything")
	if sc := res.StructuredContent.(map[string]any); sc["result"] != 3 {
		t.Fatalf("anything structuredContent = %v", sc)
	}
	if res.Meta["fastmcp"].(map[string]any)["wrap_result"] != true {
		t.Fatalf("anything _meta = %v", res.Meta)
	}
	// Unwrapped results carry no marker.
	if res := call("scores"); res.Meta != nil {
		t.Fatalf("scores _meta = %v", res.Meta)
	}
	// A returned *ToolResult passes through untouched.
	if res := call("custom"); res.Content[0].Text != "hi" || res.StructuredContent != nil {
		t.Fatalf("custom = %+v", res)
	}
}

func TestBadSignaturesPanic(t *testing.T) {
	m := New("test")
	for name, fn := range map[string]any{
		"non-struct arg":    func(a int) int { return a },
		"too many args":     func(a, b addArgs) int { return 0 },
		"bad second return": func(addArgs) (int, int) { return 0, 0 },
		"not a func":        42,
		"anonymous no name": func() {},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			m.Tool(fn)
		}()
	}
	// Anonymous functions work once named.
	m.Tool(func() string { return "ok" }, WithName("anon"))
}

func TestRunStdioRedirectsStrayPrints(t *testing.T) {
	m := New("test")
	m.Tool(func() string {
		fmt.Print("stray debug output") // no newline: would glue onto the response
		return "ok"
	}, WithName("noisy"))

	pipe := func() (*os.File, *os.File) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		return r, w
	}
	readAll := func(r *os.File) <-chan string {
		c := make(chan string, 1)
		go func() { b, _ := io.ReadAll(r); c <- string(b) }()
		return c
	}
	inR, inW := pipe()
	outR, outW := pipe()
	errR, errW := pipe()
	origIn, origOut, origErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	defer func() { os.Stdin, os.Stdout, os.Stderr = origIn, origOut, origErr }()
	stdout, stderr := readAll(outR), readAll(errR)

	fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"noisy","arguments":{}}}`)
	inW.Close()
	err := m.RunStdio(context.Background())
	restored := os.Stdout == outW
	outW.Close()
	errW.Close()
	out, errOut := <-stdout, <-stderr

	if err != nil {
		t.Fatalf("RunStdio: %v", err)
	}
	if !restored {
		t.Fatal("RunStdio should restore os.Stdout when it returns")
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("stdout must hold only protocol messages, got %q: %v", out, err)
	}
	if text := resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "ok" {
		t.Fatalf("response = %v", resp)
	}
	if errOut != "stray debug output" {
		t.Fatalf("stray print should go to stderr, got %q", errOut)
	}
}

func rpc(t *testing.T, m *FastMCP, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := m.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(l), &msg); err != nil {
			t.Fatalf("bad output line %q: %v", l, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestStdioProtocol(t *testing.T) {
	m := New("Demo", WithInstructions("be nice"))
	m.Tool(add, WithDescription("Add two numbers"))

	msgs := rpc(t, m,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"add","arguments":{"a":40,"b":2}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"bogus/method"}`,
		`not json`,
	)
	if len(msgs) != 6 {
		t.Fatalf("expected 6 responses (notification gets none), got %d: %v", len(msgs), msgs)
	}

	init := msgs[0]["result"].(map[string]any)
	if init["protocolVersion"] != ProtocolVersion || init["instructions"] != "be nice" {
		t.Fatalf("initialize = %v", init)
	}
	if init["serverInfo"].(map[string]any)["name"] != "Demo" {
		t.Fatalf("serverInfo = %v", init["serverInfo"])
	}

	tools := msgs[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "add" {
		t.Fatalf("tools/list = %v", tools)
	}
	if out := tools[0].(map[string]any)["outputSchema"].(map[string]any); out["x-fastmcp-wrap-result"] != true {
		t.Fatalf("tools/list outputSchema = %v", out)
	}

	call := msgs[2]["result"].(map[string]any)
	if call["content"].([]any)[0].(map[string]any)["text"] != "42" {
		t.Fatalf("tools/call = %v", call)
	}

	if code := msgs[3]["error"].(map[string]any)["code"]; code != float64(codeInvalidParams) {
		t.Fatalf("unknown tool code = %v", code)
	}
	if code := msgs[4]["error"].(map[string]any)["code"]; code != float64(codeMethodNotFound) {
		t.Fatalf("bogus method code = %v", code)
	}
	if code := msgs[5]["error"].(map[string]any)["code"]; code != float64(codeParseError) {
		t.Fatalf("parse error code = %v", code)
	}
}
