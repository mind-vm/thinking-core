package reliability

// Copied from agentloop/shape.go (unexported there), so arms B and C show
// the model the same shape digest the library does. Keep in step with it.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// dataShape renders a compact structural digest of a turn's return
// value: keys and types, array lengths, nested field names — but NOT
// the values. It is what the loop shows the model in place of the full
// data blob, so the model knows which fields it can reach on next
// turn's `args` without the values inflating the prompt.
//
// Rendered as a TypeScript type literal — e.g.
// { rows: { title: string }[] /* 3 items */ } — so the model reads it
// with the same instincts it applies to the ## Sandbox API declarations.
func dataShape(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return shapeOf(v, 0)
}

const (
	shapeMaxDepth = 4
	shapeMaxKeys  = 24
)

func shapeOf(v any, depth int) string {
	switch t := v.(type) {
	case map[string]any:
		if depth >= shapeMaxDepth {
			return "object"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		truncated := 0
		if len(keys) > shapeMaxKeys {
			truncated = len(keys) - shapeMaxKeys
			keys = keys[:shapeMaxKeys]
		}
		parts := make([]string, 0, len(keys)+1)
		for _, k := range keys {
			parts = append(parts, k+": "+shapeOf(t[k], depth+1))
		}
		if truncated > 0 {
			parts = append(parts, fmt.Sprintf("/* +%d more keys */", truncated))
		}
		return "{ " + strings.Join(parts, "; ") + " }"
	case []any:
		if len(t) == 0 {
			return "unknown[] /* empty */"
		}
		if depth >= shapeMaxDepth {
			return fmt.Sprintf("unknown[] /* %d items */", len(t))
		}
		return fmt.Sprintf("%s[] /* %d items */", elemShape(t[0], depth+1), len(t))
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "any"
	}
}

// elemShape renders an array's element type for the `T[]` suffix form.
// Object literals are already brace-delimited and primitives are single
// words; any other composite shape is parenthesised so the [] suffix
// binds unambiguously.
func elemShape(v any, depth int) string {
	s := shapeOf(v, depth)
	if strings.HasPrefix(s, "{") || !strings.Contains(s, " ") {
		return s
	}
	return "(" + s + ")"
}
