package reliability

import (
	"fmt"
	"strings"

	"github.com/dop251/goja"
	"github.com/mind-vm/thinking-core/sandbox"
)

// The two surfaces below are generated from the same []Function, so the
// code arms and the tool-calling arm are offered exactly the same
// capabilities, with the same names, parameters, docs and return types.

// Pack offers fns to a script as JavaScript functions that call w.
func Pack(w *World, fns []Function) sandbox.Pack {
	return sandbox.Pack{
		Name:        "shop",
		Description: "the shop's API",
		Prompt:      Declarations(fns),
		Register: func(rt *goja.Runtime, _ *sandbox.Sandbox) {
			for _, f := range fns {
				_ = rt.Set(f.Name, func(call goja.FunctionCall) goja.Value {
					args := map[string]any{}
					if f.Positional {
						for i, p := range f.Params {
							if v := call.Argument(i); !goja.IsUndefined(v) && !goja.IsNull(v) {
								args[p.Name] = v.Export()
							}
						}
					} else if len(f.Params) > 0 {
						switch m := call.Argument(0).Export().(type) {
						case map[string]any:
							args = m
						case nil:
						default:
							panic(rt.NewGoError(fmt.Errorf("%s: pass one object, e.g. %s({ %s: … })", f.Name, f.Name, f.Params[0].Name)))
						}
					}
					res, err := w.Invoke(f.Name, normalize(args))
					if err != nil {
						panic(rt.NewGoError(err))
					}
					return rt.ToValue(res)
				})
			}
		},
	}
}

// normalize turns what goja exports ([]interface{} of int64, maps) into
// what a tool call's JSON decodes to, so handlers see one shape.
func normalize(args map[string]any) map[string]any {
	if m, ok := plain(args).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Declarations renders fns as the TypeScript declare lines the system
// prompt shows the code arms.
func Declarations(fns []Function) string {
	var b strings.Builder
	for _, f := range fns {
		fmt.Fprintf(&b, "/** %s */\ndeclare function %s(%s): %s;\n", f.Doc, f.Name, tsParams(f), f.Returns)
	}
	return strings.TrimRight(b.String(), "\n")
}

func tsParams(f Function) string {
	if len(f.Params) == 0 {
		return ""
	}
	if f.Positional {
		parts := make([]string, len(f.Params))
		for i, p := range f.Params {
			parts[i] = tsField(p)
		}
		return strings.Join(parts, ", ")
	}
	return "args: " + tsObject(f.Params)
}

func tsObject(ps []Param) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = tsField(p)
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}

func tsField(p Param) string {
	opt := ""
	if p.Optional {
		opt = "?"
	}
	s := p.Name + opt + ": " + tsType(p)
	if p.Doc != "" {
		s += " /* " + p.Doc + " */"
	}
	return s
}

func tsType(p Param) string {
	switch {
	case len(p.Enum) > 0:
		q := make([]string, len(p.Enum))
		for i, e := range p.Enum {
			q[i] = fmt.Sprintf("%q", e)
		}
		return strings.Join(q, " | ")
	case p.Kind == "array":
		return p.Items + "[]"
	case p.Kind == "object":
		return tsObject(p.Object)
	}
	return p.Kind
}

// Tools renders fns as OpenAI-wire tool definitions. The return type goes
// into the description, because a tool schema has no place for it and the
// code arms are told it.
func Tools(fns []Function) []map[string]any {
	out := make([]map[string]any, 0, len(fns))
	for _, f := range fns {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        f.Name,
				"description": f.Doc + " Returns: " + f.Returns,
				"parameters":  schemaObject(f.Params),
			},
		})
	}
	return out
}

func schemaObject(ps []Param) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range ps {
		props[p.Name] = schemaOf(p)
		if !p.Optional {
			required = append(required, p.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func schemaOf(p Param) map[string]any {
	s := map[string]any{"type": p.Kind}
	if len(p.Enum) > 0 {
		s["enum"] = p.Enum
	}
	if p.Kind == "array" {
		s["items"] = map[string]any{"type": p.Items}
	}
	if p.Kind == "object" {
		s = schemaObject(p.Object)
	}
	if p.Doc != "" {
		s["description"] = p.Doc
	}
	return s
}
