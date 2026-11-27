package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Check reports whether answer — an agent's final text — carries the
// task's expected value. The empty reason means correct.
//
// It is strict about values and lenient about presentation: the JSON may
// sit in a code fence or after a sentence, keys beyond the expected ones
// are ignored, strings compare case-insensitively after trimming, and a
// number may arrive as a numeric string. What it never forgives is a
// wrong number, a wrong id, or a wrong order — the failures the benchmark
// exists to count. Formatting slips are not what either approach is
// being compared on.
func (t Task) Check(answer string) (bool, string) {
	got, err := ExtractJSON(answer)
	if err != nil {
		return false, err.Error()
	}
	if err := match("", t.Expected, got); err != nil {
		return false, err.Error()
	}
	return true, ""
}

var fenceRe = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*\\n(.*?)```")

// ExtractJSON finds the JSON object in an agent's final text: the whole
// text, else a fenced block, else the outermost {...} span.
func ExtractJSON(text string) (any, error) {
	text = strings.TrimSpace(text)
	try := func(s string) (any, bool) {
		var v any
		dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(s)))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return nil, false
		}
		_, isObj := v.(map[string]any)
		return v, isObj
	}
	if v, ok := try(text); ok {
		return v, nil
	}
	for _, m := range fenceRe.FindAllStringSubmatch(text, -1) {
		if v, ok := try(m[1]); ok {
			return v, nil
		}
	}
	if lo, hi := strings.Index(text, "{"), strings.LastIndex(text, "}"); lo >= 0 && hi > lo {
		if v, ok := try(text[lo : hi+1]); ok {
			return v, nil
		}
	}
	short := text
	if len(short) > 200 {
		short = short[:200] + "…"
	}
	return nil, fmt.Errorf("no JSON object in the answer: %q", short)
}

func match(path string, want, got any) error {
	where := path
	if where == "" {
		where = "answer"
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %s", where, brief(got))
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s: missing key %q", where, k)
			}
			if err := match(path+"."+k, wv, gv); err != nil {
				return err
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array, got %s", where, brief(got))
		}
		if len(g) != len(w) {
			return fmt.Errorf("%s: want %d entries, got %d", where, len(w), len(g))
		}
		for i := range w {
			if err := match(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); err != nil {
				return err
			}
		}
		return nil
	case int:
		n, ok := number(got)
		if !ok || n != float64(w) {
			return fmt.Errorf("%s: want %d, got %s", where, w, brief(got))
		}
		return nil
	case string:
		g, ok := got.(string)
		if !ok || !strings.EqualFold(strings.TrimSpace(g), w) {
			return fmt.Errorf("%s: want %q, got %s", where, w, brief(got))
		}
		return nil
	}
	return fmt.Errorf("%s: checker cannot compare %T", where, want)
}

func number(v any) (float64, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = string(x)
	case string:
		s = strings.TrimSpace(x)
	case float64:
		return x, true
	default:
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

func brief(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
