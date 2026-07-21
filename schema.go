package loom

// schema.go — structured output (`loom run --output-schema file.json`): validate a
// worker's FINAL answer against a JSON Schema, so a foreman parses a contract
// instead of regex-mining prose (native-harness parity #4).
//
// The validator is a deliberately MINIMAL, hand-rolled subset of JSON Schema —
// loom is zero-dependency, so no schema library. Supported keywords:
//
//	type (string or array of strings; "integer" means an integral number)
//	properties, required            (objects; unknown keys are ALLOWED — no
//	                                 additionalProperties enforcement)
//	items                           (arrays: one schema applied to every element)
//	enum                            (deep-equal against the decoded JSON values)
//	minimum, maximum                (numbers, inclusive)
//	minLength, maxLength            (strings, in runes)
//	minItems, maxItems              (arrays)
//
// Anything else in the schema file ($ref, oneOf, pattern, format, …) is IGNORED,
// not rejected — a schema using them still validates on the subset it shares
// with this list. Keep foreman contracts inside the subset.
//
// Models wrap JSON in prose and code fences, so extraction is tolerant (see
// ExtractJSONValue): the whole reply as one JSON value, else the first ```fenced
// block that decodes, else the first decodable {…} / […] in the text.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Schema is one node of the parsed schema tree (the subset above).
type Schema struct {
	Types      []string           // empty = any type
	Properties map[string]*Schema // object member schemas
	Required   []string           // object members that must be present
	Items      *Schema            // array element schema
	Enum       []any              // decoded allowed values
	Minimum    *float64           // numbers: v >= Minimum
	Maximum    *float64           // numbers: v <= Maximum
	MinLength  *int               // strings: rune count >= MinLength
	MaxLength  *int               // strings: rune count <= MaxLength
	MinItems   *int               // arrays: len >= MinItems
	MaxItems   *int               // arrays: len <= MaxItems

	raw json.RawMessage // the schema node as authored (the root's raw = the whole file)
}

// ParseSchemaFile reads and parses a JSON-Schema-subset file.
func ParseSchemaFile(path string) (*Schema, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("output-schema: %w", err)
	}
	s, err := ParseSchema(b)
	if err != nil {
		return nil, fmt.Errorf("output-schema %s: %w", path, err)
	}
	return s, nil
}

// ParseSchema parses one schema document (or sub-schema) from JSON bytes.
func ParseSchema(b []byte) (*Schema, error) {
	var node struct {
		Type       json.RawMessage            `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Items      json.RawMessage            `json:"items"`
		Enum       []any                      `json:"enum"`
		Minimum    *float64                   `json:"minimum"`
		Maximum    *float64                   `json:"maximum"`
		MinLength  *int                       `json:"minLength"`
		MaxLength  *int                       `json:"maxLength"`
		MinItems   *int                       `json:"minItems"`
		MaxItems   *int                       `json:"maxItems"`
	}
	if err := json.Unmarshal(b, &node); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	s := &Schema{
		Required: node.Required, Enum: node.Enum,
		Minimum: node.Minimum, Maximum: node.Maximum,
		MinLength: node.MinLength, MaxLength: node.MaxLength,
		MinItems: node.MinItems, MaxItems: node.MaxItems,
		raw: append(json.RawMessage(nil), b...),
	}
	if len(node.Type) > 0 {
		var one string
		var many []string
		switch {
		case json.Unmarshal(node.Type, &one) == nil:
			s.Types = []string{one}
		case json.Unmarshal(node.Type, &many) == nil:
			s.Types = many
		default:
			return nil, fmt.Errorf("parse schema: \"type\" must be a string or array of strings")
		}
		for _, t := range s.Types {
			switch t {
			case "object", "array", "string", "number", "integer", "boolean", "null":
			default:
				return nil, fmt.Errorf("parse schema: unknown type %q", t)
			}
		}
	}
	if len(node.Properties) > 0 {
		s.Properties = make(map[string]*Schema, len(node.Properties))
		for k, raw := range node.Properties {
			sub, err := ParseSchema(raw)
			if err != nil {
				return nil, fmt.Errorf("properties.%s: %w", k, err)
			}
			s.Properties[k] = sub
		}
	}
	if len(node.Items) > 0 {
		sub, err := ParseSchema(node.Items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		s.Items = sub
	}
	return s, nil
}

// Raw returns the schema node exactly as authored (for embedding in prompts).
func (s *Schema) Raw() json.RawMessage { return s.raw }

// Validate checks a decoded JSON value (json.Unmarshal into any) against the
// schema and returns every violation as "path: problem" strings. Empty = valid.
func (s *Schema) Validate(v any) []string {
	var errs []string
	s.validate("$", v, &errs)
	return errs
}

func (s *Schema) validate(path string, v any, errs *[]string) {
	if len(s.Types) > 0 && !typeMatches(s.Types, v) {
		*errs = append(*errs, fmt.Sprintf("%s: is %s, want %s", path, jsonTypeName(v), strings.Join(s.Types, " or ")))
		return // wrong shape — deeper checks would only cascade noise
	}
	if len(s.Enum) > 0 {
		ok := false
		for _, e := range s.Enum {
			if reflect.DeepEqual(e, v) {
				ok = true
				break
			}
		}
		if !ok {
			allowed, _ := json.Marshal(s.Enum)
			*errs = append(*errs, fmt.Sprintf("%s: value not in enum %s", path, allowed))
		}
	}
	switch val := v.(type) {
	case map[string]any:
		for _, req := range s.Required {
			if _, ok := val[req]; !ok {
				*errs = append(*errs, fmt.Sprintf("%s: missing required property %q", path, req))
			}
		}
		for k, sub := range s.Properties {
			if member, ok := val[k]; ok {
				sub.validate(path+"."+k, member, errs)
			}
		}
	case []any:
		if s.MinItems != nil && len(val) < *s.MinItems {
			*errs = append(*errs, fmt.Sprintf("%s: has %d items, want at least %d", path, len(val), *s.MinItems))
		}
		if s.MaxItems != nil && len(val) > *s.MaxItems {
			*errs = append(*errs, fmt.Sprintf("%s: has %d items, want at most %d", path, len(val), *s.MaxItems))
		}
		if s.Items != nil {
			for i, item := range val {
				s.Items.validate(fmt.Sprintf("%s[%d]", path, i), item, errs)
			}
		}
	case string:
		n := utf8.RuneCountInString(val)
		if s.MinLength != nil && n < *s.MinLength {
			*errs = append(*errs, fmt.Sprintf("%s: length %d, want at least %d", path, n, *s.MinLength))
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			*errs = append(*errs, fmt.Sprintf("%s: length %d, want at most %d", path, n, *s.MaxLength))
		}
	case float64:
		if s.Minimum != nil && val < *s.Minimum {
			*errs = append(*errs, fmt.Sprintf("%s: %v is below minimum %v", path, val, *s.Minimum))
		}
		if s.Maximum != nil && val > *s.Maximum {
			*errs = append(*errs, fmt.Sprintf("%s: %v is above maximum %v", path, val, *s.Maximum))
		}
	}
}

// typeMatches reports whether the decoded value satisfies ANY of the listed
// JSON-Schema type names. "integer" is a number with an integral value (JSON has
// no integer type; 3.0 decodes to float64(3), which IS an integer).
func typeMatches(types []string, v any) bool {
	for _, t := range types {
		switch t {
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "number":
			if _, ok := v.(float64); ok {
				return true
			}
		case "integer":
			if f, ok := v.(float64); ok && f == math.Trunc(f) && !math.IsInf(f, 0) {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "null":
			if v == nil {
				return true
			}
		}
	}
	return false
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// ExtractJSONValue finds the FIRST JSON value in a model's reply text. Models
// wrap JSON in prose and code fences, so the search is tolerant, in order:
//
//  1. the whole trimmed text decodes as one JSON value → that value
//  2. each ``` fenced block (```json or bare), in order → the first that decodes
//  3. the first '{' or '[' from which a complete JSON value decodes → that value
//
// The returned bytes are exactly the decoded segment (re-sliceable as
// json.RawMessage). A reply with no decodable JSON returns an error.
func ExtractJSONValue(text string) (json.RawMessage, error) {
	if raw, ok := decodeOneJSON(strings.TrimSpace(text)); ok {
		return raw, nil
	}
	for _, block := range fencedBlocks(text) {
		if raw, ok := decodeOneJSON(strings.TrimSpace(block)); ok {
			return raw, nil
		}
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' && text[i] != '[' {
			continue
		}
		if raw, ok := decodeOneJSON(text[i:]); ok {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("no JSON value found in the reply")
}

// decodeOneJSON decodes the first complete JSON value at the start of s and
// returns exactly its bytes. Trailing content after the value is fine (prose
// following an object); leading garbage is not (the caller positions s).
func decodeOneJSON(s string) (json.RawMessage, bool) {
	if s == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	end := dec.InputOffset()
	return json.RawMessage(strings.TrimSpace(s[:end])), true
}

// fencedBlocks returns the contents of every ``` code fence in order, with any
// language tag on the opening line dropped. An unclosed final fence yields its
// remainder (a truncated reply should still surface its JSON).
func fencedBlocks(text string) []string {
	var blocks []string
	rest := text
	for {
		open := strings.Index(rest, "```")
		if open < 0 {
			return blocks
		}
		rest = rest[open+3:]
		// drop the info string ("json", "jsonc", …) up to the first newline
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		} else {
			return blocks // ``` at EOF with no content
		}
		close := strings.Index(rest, "```")
		if close < 0 {
			return append(blocks, rest)
		}
		blocks = append(blocks, rest[:close])
		rest = rest[close+3:]
	}
}

// SchemaPromptSuffix is appended to an outgoing prompt when --output-schema is
// set, so the worker KNOWS the contract it must answer in — validating an
// answer against a schema the model never saw would burn the one retry on
// guessing. The authored schema rides verbatim.
func SchemaPromptSuffix(s *Schema) string {
	return "\n\nYour FINAL answer must be a single JSON value matching this JSON Schema. " +
		"Reply with ONLY the JSON (a ```json fence is acceptable):\n" + string(s.Raw())
}

// schemaRetryPrompt frames the ONE re-prompt after a failed validation.
func schemaRetryPrompt(s *Schema, errs []string) string {
	return "Your answer failed schema validation: " + strings.Join(errs, "; ") +
		". Reply with ONLY valid JSON matching the schema:\n" + string(s.Raw())
}

// EnforceSchema validates a turn's reply text against the schema. On success the
// extracted JSON lands in Reply.Parsed. On failure it re-prompts ONCE on the same
// session ("your answer failed validation: …"); a still-invalid answer returns the
// final reply plus a non-nil error listing the violations. The retry turn's
// usage/cost is folded into the returned Reply so accounting never loses a turn.
// A nil-Exceeded budget gates the retry: past the ceiling, the invalid answer is
// returned immediately (with its errors) rather than spending another turn.
func EnforceSchema(ctx context.Context, sess Session, s *Schema, r Reply, budget *Budget, onEvent func(Event)) (Reply, error) {
	raw, errs := checkText(s, r.Text)
	if len(errs) == 0 {
		r.Parsed = raw
		return r, nil
	}
	if !budget.Allow() {
		return r, fmt.Errorf("schema validation failed (%s) and the budget is exhausted — no retry", strings.Join(errs, "; "))
	}
	retry, err := sendMaybeStream(ctx, sess, schemaRetryPrompt(s, errs), onEvent)
	budget.Note(retry)
	// fold the first turn's accounting into the reply we return
	retry.CostUSD += r.CostUSD
	retry.Turns += r.Turns
	retry.Usage = addUsage(r.Usage, retry.Usage)
	if retry.SessionID == "" {
		retry.SessionID = r.SessionID
	}
	if err != nil {
		return retry, fmt.Errorf("schema retry turn: %w", err)
	}
	raw, errs = checkText(s, retry.Text)
	if len(errs) == 0 {
		retry.Parsed = raw
		return retry, nil
	}
	return retry, fmt.Errorf("schema validation failed after retry: %s", strings.Join(errs, "; "))
}

// checkText extracts the reply's JSON and validates it; extraction failure is
// reported as a violation (same retry path as a shape mismatch).
func checkText(s *Schema, text string) (json.RawMessage, []string) {
	raw, err := ExtractJSONValue(text)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var v any
	if uerr := json.Unmarshal(raw, &v); uerr != nil {
		return nil, []string{"extracted JSON did not re-parse: " + uerr.Error()}
	}
	if errs := s.Validate(v); len(errs) > 0 {
		return nil, errs
	}
	return raw, nil
}

// sendMaybeStream mirrors the CLI's sendTurn split: stream when an event sink
// wants the turn's work, else the plain final-text path.
func sendMaybeStream(ctx context.Context, sess Session, prompt string, onEvent func(Event)) (Reply, error) {
	if onEvent != nil {
		return sess.SendStream(ctx, prompt, onEvent)
	}
	return sess.Send(ctx, prompt)
}
