package loom

// EventKind is the type of a streamed event from a backend during a turn.
type EventKind string

const (
	EvAssistant  EventKind = "assistant"   // assistant message text
	EvThinking   EventKind = "thinking"    // reasoning / chain-of-thought
	EvToolCall   EventKind = "tool_call"   // the agent invoked a tool
	EvToolResult EventKind = "tool_result" // a tool returned output
	EvResult     EventKind = "result"      // the final result of the turn
)

// Event is a single streamed event during a turn. An onEvent callback (passed to
// SendStream) receives these as they arrive; the final Reply is still returned.
// This is what turns loom from a black box (final text only) into an observable
// harness — a code-review agent's *work* (which files it read, what it found)
// becomes visible, not hidden.
type Event struct {
	Kind    EventKind `json:"kind"`
	Backend string    `json:"backend"`
	Text    string    `json:"text,omitempty"` // message / thinking / result text
	Tool    string    `json:"tool,omitempty"` // tool name (tool_call / tool_result)
}

// emit is a nil-safe callback invoker used by backends.
func emit(onEvent func(Event), ev Event) {
	if onEvent != nil {
		onEvent(ev)
	}
}
