package fastmcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// schemaMode says which side of a tool call a schema describes.
//
// Input schemas tell the model what it may send, so they describe the values
// we want. Output schemas are validated by clients against what encoding/json
// actually produces, and Go encodes a nil pointer, slice or map as null, so
// those positions must also accept null.
type schemaMode int

const (
	inputSchema schemaMode = iota
	outputSchema
)

var timeType = reflect.TypeOf(time.Time{})

// schemaBuilder generates JSON Schema from Go types. Self-referencing structs
// (type Node struct { Children []*Node }) would recurse forever if inlined, so
// they are emitted once under $defs and referenced with $ref, the same way
// Pydantic does it for the Python framework.
type schemaBuilder struct {
	mode      schemaMode
	visiting  map[reflect.Type]bool   // structs currently being expanded
	recursive map[reflect.Type]bool   // structs that refer back to themselves
	names     map[reflect.Type]string // $defs name for each recursive struct
	defs      map[string]any
}

func newSchemaBuilder(mode schemaMode) *schemaBuilder {
	return &schemaBuilder{
		mode:      mode,
		visiting:  map[reflect.Type]bool{},
		recursive: map[reflect.Type]bool{},
		names:     map[reflect.Type]string{},
		defs:      map[string]any{},
	}
}

// root returns the schema for t as a document root: a recursive struct is
// inlined rather than left as a bare $ref, and any $defs are attached.
func (b *schemaBuilder) root(t reflect.Type) map[string]any {
	s := b.typeSchema(t)
	if t.Kind() == reflect.Struct && b.recursive[t] {
		s = copySchema(b.defs[b.names[t]].(map[string]any))
	}
	return b.withDefs(s)
}

// withDefs attaches the collected $defs to a root schema.
func (b *schemaBuilder) withDefs(s map[string]any) map[string]any {
	if len(b.defs) > 0 {
		s["$defs"] = b.defs
	}
	return s
}

// schemaForArgs builds a JSON Schema object describing a tool's args struct.
// A nil type yields an empty object schema (a tool with no inputs).
func schemaForArgs(t reflect.Type) map[string]any {
	if t == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return newSchemaBuilder(inputSchema).root(t)
}

// nestedSchema is the schema for a value inside a struct, slice, or map.
func (b *schemaBuilder) nestedSchema(t reflect.Type) map[string]any {
	s := b.typeSchema(t)
	if b.mode == outputSchema && canBeNil(t) {
		return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
	}
	return s
}

func canBeNil(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return true
	}
	return false
}

func (b *schemaBuilder) typeSchema(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t.Kind() == reflect.Pointer:
		return b.typeSchema(t.Elem())
	}

	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": b.nestedSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": b.nestedSchema(t.Elem())}
	case reflect.Struct:
		return b.structRef(t)
	case reflect.Interface:
		return map[string]any{} // any JSON value
	default:
		return map[string]any{}
	}
}

// structRef returns a struct's schema inline, or a $ref when the struct is
// recursive.
func (b *schemaBuilder) structRef(t reflect.Type) map[string]any {
	if b.visiting[t] {
		// We are already inside t: this is a self-reference.
		b.recursive[t] = true
		return b.ref(t)
	}
	if b.recursive[t] {
		return b.ref(t) // already emitted under $defs
	}
	b.visiting[t] = true
	s := b.structSchema(t)
	delete(b.visiting, t)

	if b.recursive[t] {
		b.defs[b.defName(t)] = s
		return b.ref(t)
	}
	return s
}

func (b *schemaBuilder) ref(t reflect.Type) map[string]any {
	return map[string]any{"$ref": "#/$defs/" + b.defName(t)}
}

// defName picks a $defs key for t: its Go type name, qualified with the
// package path only if another type already uses that name.
func (b *schemaBuilder) defName(t reflect.Type) string {
	if n, ok := b.names[t]; ok {
		return n
	}
	name := t.Name()
	for _, used := range b.names {
		if used == name {
			name = strings.NewReplacer("/", "_", ".", "_").Replace(t.PkgPath()) + "_" + t.Name()
			break
		}
	}
	b.names[t] = name
	return name
}

func (b *schemaBuilder) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	required := []string{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, omitEmpty, skip := jsonName(f)
		if skip {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct && !hasJSONTag(f) {
			// Embedded struct: flatten its fields, as encoding/json does.
			inner := b.structSchema(f.Type)
			for k, v := range inner["properties"].(map[string]any) {
				props[k] = v
			}
			if r, ok := inner["required"].([]string); ok {
				required = append(required, r...)
			}
			continue
		}

		prop := b.nestedSchema(f.Type)
		if _, isRef := prop["$ref"]; isRef {
			// Keywords beside $ref are ignored by older validators, so wrap
			// the reference before annotating it.
			prop = map[string]any{"allOf": []any{prop}}
		}
		if d := f.Tag.Get("description"); d != "" {
			prop["description"] = d
		}
		// default and enum constrain what the model sends; they are input-only.
		if def := f.Tag.Get("default"); def != "" && b.mode == inputSchema {
			var v any
			if err := json.Unmarshal([]byte(def), &v); err == nil {
				prop["default"] = v
			} else {
				prop["default"] = def
			}
			omitEmpty = true
		}
		if e := f.Tag.Get("enum"); e != "" && b.mode == inputSchema {
			vals := strings.Split(e, ",")
			enum := make([]any, len(vals))
			for i, v := range vals {
				enum[i] = strings.TrimSpace(v)
			}
			prop["enum"] = enum
		}
		props[name] = prop

		if !omitEmpty && f.Type.Kind() != reflect.Pointer {
			required = append(required, name)
		}
	}

	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// copySchema makes a shallow copy so a root schema can gain $defs without
// the same map also appearing (and nesting) inside $defs.
func copySchema(s map[string]any) map[string]any {
	out := make(map[string]any, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	return out
}

func hasJSONTag(f reflect.StructField) bool {
	_, ok := f.Tag.Lookup("json")
	return ok
}

// jsonName resolves the property name the same way encoding/json does.
func jsonName(f reflect.StructField) (name string, omitEmpty, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" || p == "omitzero" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}
