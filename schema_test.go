package loom

// schema_test.go — the structured-output subset (schema.go): parsing, tolerant
// JSON extraction, validation, and the enforce-with-one-retry loop driven by a
// hermetic scripted session (no process, no money).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const verdictSchema = `{
  "type": "object",
  "required": ["verdict", "confidence"],
  "properties": {
    "verdict":    {"type": "string", "enum": ["pass", "fail"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "notes":      {"type": "array", "items": {"type": "string"}, "maxItems": 3}
  }
}`

func mustSchema(t *testing.T, src string) *Schema {
	t.Helper()
	s, err := ParseSchema([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSchemaValidate(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	cases := []struct {
		name, doc string
		wantErrs  int
	}{
		{"valid", `{"verdict":"pass","confidence":0.9}`, 0},
		{"valid with notes", `{"verdict":"fail","confidence":0,"notes":["a","b"]}`, 0},
		{"unknown keys allowed", `{"verdict":"pass","confidence":1,"extra":42}`, 0},
		{"missing required", `{"verdict":"pass"}`, 1},
		{"enum violation", `{"verdict":"maybe","confidence":0.5}`, 1},
		{"range violation", `{"verdict":"pass","confidence":1.5}`, 1},
		{"wrong member type", `{"verdict":7,"confidence":0.5}`, 1},
		{"wrong root type", `["pass"]`, 1},
		{"too many items", `{"verdict":"pass","confidence":1,"notes":["a","b","c","d"]}`, 1},
		{"item type", `{"verdict":"pass","confidence":1,"notes":[1]}`, 1},
		{"two independent violations", `{"verdict":"maybe","confidence":2}`, 2},
	}
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.doc), &v); err != nil {
			t.Fatalf("%s: bad fixture: %v", c.name, err)
		}
		if errs := s.Validate(v); len(errs) != c.wantErrs {
			t.Errorf("%s: got %d violations %v, want %d", c.name, len(errs), errs, c.wantErrs)
		}
	}
}

func TestSchemaTypesAndInteger(t *testing.T) {
	s := mustSchema(t, `{"type":"integer"}`)
	if errs := s.Validate(float64(3)); len(errs) != 0 {
		t.Errorf("3 is an integer: %v", errs)
	}
	if errs := s.Validate(float64(3.5)); len(errs) == 0 {
		t.Error("3.5 is not an integer")
	}
	multi := mustSchema(t, `{"type":["string","null"]}`)
	if errs := multi.Validate(nil); len(errs) != 0 {
		t.Errorf("null allowed by [string,null]: %v", errs)
	}
	if errs := multi.Validate(float64(1)); len(errs) == 0 {
		t.Error("number rejected by [string,null]")
	}
	strSchema := mustSchema(t, `{"type":"string","minLength":2,"maxLength":3}`)
	if errs := strSchema.Validate("ab"); len(errs) != 0 {
		t.Errorf("2 runes within [2,3]: %v", errs)
	}
	if errs := strSchema.Validate("a"); len(errs) == 0 {
		t.Error("1 rune below minLength must fail")
	}
	if errs := strSchema.Validate("abcd"); len(errs) == 0 {
		t.Error("4 runes above maxLength must fail")
	}
}

func TestSchemaParseErrors(t *testing.T) {
	if _, err := ParseSchema([]byte(`not json`)); err == nil {
		t.Error("non-JSON schema must fail to parse")
	}
	if _, err := ParseSchema([]byte(`{"type":"wobble"}`)); err == nil {
		t.Error("unknown type name must fail to parse")
	}
	if _, err := ParseSchema([]byte(`{"type":42}`)); err == nil {
		t.Error("numeric type must fail to parse")
	}
	if _, err := ParseSchema([]byte(`{"type":"object","properties":{"x":{"type":"wobble"}}}`)); err == nil {
		t.Error("nested bad type must fail to parse")
	}
	// file-level errors: missing file, unreadable JSON
	if _, err := ParseSchemaFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing schema file must error")
	}
	p := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(p, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSchemaFile(p); err == nil {
		t.Error("truncated schema file must error")
	}
	p2 := filepath.Join(t.TempDir(), "ok.json")
	if err := os.WriteFile(p2, []byte(verdictSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := ParseSchemaFile(p2); err != nil || s == nil {
		t.Errorf("good file: %v", err)
	}
}

func TestExtractJSONValue(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"bare object", `{"a":1}`, `{"a":1}`},
		{"whitespace", "  \n {\"a\":1} \n", `{"a":1}`},
		{"json fence", "Here you go:\n```json\n{\"a\": 1}\n```\nHope that helps!", `{"a": 1}`},
		{"bare fence", "```\n[1,2]\n```", `[1,2]`},
		{"unclosed fence", "```json\n{\"a\":1}", `{"a":1}`},
		{"object mid-prose", `The result is {"a":1} as requested.`, `{"a":1}`},
		{"prose brace then object", `Use {curly} wisely: {"a":1} done.`, `{"a":1}`},
		{"array mid-prose", `Values: [1,2,3].`, `[1,2,3]`},
		{"first of two", `{"a":1} and later {"b":2}`, `{"a":1}`},
		{"whole text is a JSON string", `"just a string"`, `"just a string"`},
		{"nested braces", `{"a":{"b":[1,{"c":2}]}}`, `{"a":{"b":[1,{"c":2}]}}`},
	}
	for _, c := range cases {
		got, err := ExtractJSONValue(c.text)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	for _, text := range []string{"", "no json here", "{broken", "``` \n not json \n ```"} {
		if got, err := ExtractJSONValue(text); err == nil {
			t.Errorf("%q: expected no-JSON error, got %q", text, got)
		}
	}
}

// scriptedSession is the hermetic fake: each Send pops the next scripted reply.
type scriptedSession struct {
	replies []Reply
	prompts []string
}

func (s *scriptedSession) Send(ctx context.Context, prompt string) (Reply, error) {
	return s.SendStream(ctx, prompt, nil)
}

func (s *scriptedSession) SendStream(ctx context.Context, prompt string, onEvent func(Event)) (Reply, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.replies) == 0 {
		return Reply{Backend: "fake", Err: "script exhausted"}, nil
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	emit(onEvent, Event{Kind: EvResult, Backend: "fake", Text: r.Text})
	return r, nil
}

func (s *scriptedSession) SessionID() string { return "fake-session" }
func (s *scriptedSession) Close() error      { return nil }

func TestEnforceSchemaValidFirstTry(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	sess := &scriptedSession{}
	first := Reply{Backend: "fake", Text: "Verdict below.\n```json\n{\"verdict\":\"pass\",\"confidence\":0.8}\n```"}
	got, err := EnforceSchema(context.Background(), sess, s, first, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Parsed) != `{"verdict":"pass","confidence":0.8}` {
		t.Errorf("Parsed = %s", got.Parsed)
	}
	if len(sess.prompts) != 0 {
		t.Errorf("a valid first answer must not re-prompt (sent %v)", sess.prompts)
	}
}

func TestEnforceSchemaRetryThenValid(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	sess := &scriptedSession{replies: []Reply{
		{Backend: "fake", Text: `{"verdict":"pass","confidence":0.7}`,
			CostUSD: 0.02, Turns: 1, Usage: &Usage{OutputTokens: 5, CostUSD: 0.02, CostSource: CostReal}},
	}}
	first := Reply{Backend: "fake", Text: `{"verdict":"maybe","confidence":0.7}`,
		CostUSD: 0.03, Turns: 1, Usage: &Usage{OutputTokens: 9, CostUSD: 0.03, CostSource: CostReal}}
	got, err := EnforceSchema(context.Background(), sess, s, first, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Parsed) != `{"verdict":"pass","confidence":0.7}` {
		t.Errorf("Parsed = %s", got.Parsed)
	}
	if len(sess.prompts) != 1 {
		t.Fatalf("exactly one retry, got %d", len(sess.prompts))
	}
	rp := sess.prompts[0]
	if !strings.Contains(rp, "failed schema validation") || !strings.Contains(rp, "enum") || !strings.Contains(rp, `"required"`) {
		t.Errorf("retry prompt must carry the violations AND the schema:\n%s", rp)
	}
	// accounting folds BOTH turns
	if got.CostUSD != 0.05 || got.Turns != 2 || got.Usage.OutputTokens != 14 {
		t.Errorf("folded accounting: cost=%v turns=%d usage=%+v", got.CostUSD, got.Turns, got.Usage)
	}
}

func TestEnforceSchemaRetryStillInvalid(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	sess := &scriptedSession{replies: []Reply{{Backend: "fake", Text: `still prose, no json`}}}
	first := Reply{Backend: "fake", Text: `{"verdict":"maybe","confidence":0.7}`}
	got, err := EnforceSchema(context.Background(), sess, s, first, nil, nil)
	if err == nil {
		t.Fatal("still-invalid retry must return an error")
	}
	if !strings.Contains(err.Error(), "after retry") {
		t.Errorf("error should say the retry happened: %v", err)
	}
	if got.Parsed != nil {
		t.Errorf("no Parsed on failure: %s", got.Parsed)
	}
	if len(sess.prompts) != 1 {
		t.Fatalf("exactly ONE retry even when it fails, got %d", len(sess.prompts))
	}
}

func TestEnforceSchemaBudgetGatesRetry(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	sess := &scriptedSession{replies: []Reply{{Backend: "fake", Text: `{"verdict":"pass","confidence":1}`}}}
	b := NewBudget(0.01)
	b.Note(Reply{Usage: &Usage{CostUSD: 0.02, CostSource: CostReal}}) // already over
	first := Reply{Backend: "fake", Text: `no json at all`}
	_, err := EnforceSchema(context.Background(), sess, s, first, b, nil)
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("exhausted budget must refuse the retry: %v", err)
	}
	if len(sess.prompts) != 0 {
		t.Fatalf("no retry turn may be spent past the budget, got %d", len(sess.prompts))
	}
}

func TestSchemaPromptSuffix(t *testing.T) {
	s := mustSchema(t, verdictSchema)
	suffix := SchemaPromptSuffix(s)
	if !strings.Contains(suffix, `"verdict"`) || !strings.Contains(suffix, "JSON Schema") {
		t.Errorf("suffix must carry the authored schema: %s", suffix)
	}
}
