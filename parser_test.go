package agentloop

import "testing"

func TestExtractJSBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single javascript block",
			in:   "```javascript\nlog(\"hi\")\n```",
			want: `log("hi")`,
		},
		{
			name: "js alias",
			in:   "Sure.\n```js\nvar x = 1\n```",
			want: "var x = 1",
		},
		{
			name: "trims surrounding whitespace inside fence",
			in:   "```javascript\n\n   log(1)   \n\n```",
			want: "log(1)",
		},
		{
			name: "no fence returns empty",
			in:   "Just plain prose.",
			want: "",
		},
		{
			name: "wrong language returns empty",
			in:   "```python\nprint(1)\n```",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractJSBlock(tc.in); got != tc.want {
				t.Errorf("ExtractJSBlock: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractDoneMarker(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantDone  bool
		wantFinal string
	}{
		{
			name:      "DONE alone",
			in:        "DONE",
			wantDone:  true,
			wantFinal: "",
		},
		{
			name:      "DONE then answer",
			in:        "DONE\nHere is the answer.",
			wantDone:  true,
			wantFinal: "Here is the answer.",
		},
		{
			name:      "leading whitespace tolerated",
			in:        "  DONE\nReply",
			wantDone:  true,
			wantFinal: "Reply",
		},
		{
			name:      "inline DONE in prose ignored",
			in:        "I will be DONE shortly.",
			wantDone:  false,
			wantFinal: "",
		},
		{
			name:      "no marker",
			in:        "Just talking",
			wantDone:  false,
			wantFinal: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			done, final := ExtractDoneMarker(tc.in)
			if done != tc.wantDone {
				t.Errorf("done: got %v, want %v", done, tc.wantDone)
			}
			if final != tc.wantFinal {
				t.Errorf("final: got %q, want %q", final, tc.wantFinal)
			}
		})
	}
}

// TestExtractDataBlocks: every fenced block but the script comes back raw,
// backticks and all, and a longer fence can hold a shorter one.
func TestExtractDataBlocks(t *testing.T) {
	reply := "```javascript\nwriteFile(\"a.go\", blocks[0]);\n```\n\n```go\nvar re = regexp.MustCompile(`^#`)\n```\n\n````markdown\nUse `MaxNodes`:\n```go\nx := 1\n```\n````\n"
	got := ExtractDataBlocks(reply)
	want := []string{"var re = regexp.MustCompile(`^#`)", "Use `MaxNodes`:\n```go\nx := 1\n```"}
	if len(got) != len(want) {
		t.Fatalf("got %d blocks %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d = %q, want %q", i, got[i], want[i])
		}
	}
	if blocks := ExtractDataBlocks("```js\nlog(1)\n```"); blocks == nil || len(blocks) != 0 {
		t.Errorf("script-only reply: got %#v, want an empty non-nil slice", blocks)
	}
}

// TestExtractFileBlocks: a fence labelled with a path is a file in any of
// the forms models use; an unlabelled or non-path label is not.
func TestExtractFileBlocks(t *testing.T) {
	reply := "```javascript\nconsole.log(exec([\"go\",\"test\"]).stdout)\n```\n" +
		"```go memory_test.go\npackage x\n```\n" +
		"```markdown answer\nnot a file\n```\n" +
		"```go:internal/a.go\npackage a\n```\n" +
		"```go title=\"b.go\"\npackage b\n```\n" +
		"```go\npackage unlabelled\n```\n"
	got := ExtractFileBlocks(reply)
	want := []FileBlock{{"memory_test.go", 0}, {"internal/a.go", 2}, {"b.go", 3}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if blocks := ExtractDataBlocks(reply); len(blocks) != 5 || blocks[0] != "package x" {
		t.Errorf("data blocks = %q", blocks)
	}
}
