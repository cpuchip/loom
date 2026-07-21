package loom

// usage.go — uniform per-turn usage accounting + the spend budget (native-harness
// parity #3). Each backend reports what its CLI actually emits, NORMALIZED to one
// struct so a foreman reads one shape; the CostSource marker keeps the fidelity
// honest (never present a token estimate as a dollar fact).
//
// What each backend really carries (live-captured 2026-07-20, this box):
//
//	claude   result event: usage{input_tokens, cache_creation_input_tokens,
//	         cache_read_input_tokens, output_tokens} — PER TURN — plus CUMULATIVE
//	         total_cost_usd (loom already deltas it per turn). input_tokens
//	         EXCLUDES cache reads. CostSource=real.
//	codex    turn.completed: usage{input_tokens, cached_input_tokens,
//	         output_tokens, reasoning_output_tokens}. input_tokens INCLUDES the
//	         cached portion, so loom subtracts it to keep InputTokens = fresh
//	         input across backends. Tokens only, no USD. CostSource=none.
//	copilot  result: usage{premiumRequests, …durations, codeChanges} — NO token
//	         counts there; assistant.message events carry outputTokens each.
//	         loom sums those (output only) + records premiumRequests.
//	         CostSource=none.
//	opencode step_finish: part.tokens{input, output, reasoning, cache{read,
//	         write}} + part.cost (real USD). Summed across steps. CostSource=real.
//	local    OpenAI usage{prompt_tokens, completion_tokens} when the server sends
//	         it. CostSource=none (a local model's cost is not the API's to state).
//	agy      nothing machine-readable today. CostSource=none, no fields.
//
// "estimated" is reserved for a future price-table experiment; nothing sets it
// yet — an estimate we can't keep current is worse than an honest "none".

import (
	"fmt"
	"sync"
)

// Cost-source markers for Usage.CostSource.
const (
	CostReal      = "real"      // the CLI reported spend in USD
	CostEstimated = "estimated" // derived from tokens by a price table (unused today)
	CostNone      = "none"      // no cost signal — tokens (at most) are the truth
)

// Usage is one turn's normalized resource accounting. Zero-valued fields simply
// mean "this backend doesn't report that" — see the fidelity table above.
type Usage struct {
	InputTokens      int     `json:"input_tokens,omitempty"`      // fresh (non-cache-read) input tokens
	OutputTokens     int     `json:"output_tokens,omitempty"`     // includes reasoning output where the CLI folds it in
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"` // prompt tokens served from cache
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	PremiumRequests  int     `json:"premium_requests,omitempty"` // copilot's billing unit
	CostUSD          float64 `json:"cost_usd,omitempty"`         // THIS turn (delta), when CostSource=real
	CostSource       string  `json:"cost_source,omitempty"`      // real | estimated | none
}

// TotalTokens is the budget's token denominator: everything the turn moved
// through the model. Cache reads count (they are real context the provider
// meters, just cheaper) — the point is a hard ceiling, not a bill.
func (u *Usage) TotalTokens() int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// addUsage folds two turns' usage into one record (schema retries, duo rounds).
// nil-safe on both sides; the sum's CostSource keeps the stronger claim only if
// BOTH sides make it (a real+none sum is not all-real — mark it none so a
// partial dollar figure is never presented as the whole story… except that the
// dollars themselves still accumulate for what WAS reported).
func addUsage(a, b *Usage) *Usage {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	sum := &Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		PremiumRequests:  a.PremiumRequests + b.PremiumRequests,
		CostUSD:          a.CostUSD + b.CostUSD,
	}
	if a.CostSource == b.CostSource {
		sum.CostSource = a.CostSource
	} else {
		sum.CostSource = CostNone
	}
	return sum
}

// Budget is a spend ceiling across the turns of a loop (chat, duo, flow — and
// the one schema retry of a run). ONE numeric limit, interpreted per turn by
// what that turn honestly reports:
//
//   - a turn with real USD cost counts its DOLLARS against the limit
//   - a turn with tokens only counts its TOTAL TOKENS against the limit
//   - a turn reporting neither counts nothing (agy) — the budget cannot see it
//
// The ceiling trips when EITHER accumulated dollars OR accumulated tokens cross
// the limit. A mixed loop (claude worker + codex critic) therefore holds both
// meters against the same number — document the units in the flag you expose
// ("USD for cost-reporting backends, tokens otherwise"). Exceeded() flips AFTER
// the turn that crossed the line (loom refuses FURTHER turns; it does not
// abort a turn mid-flight). Nil-safe: a nil *Budget never refuses.
type Budget struct {
	mu     sync.Mutex
	limit  float64
	usd    float64
	tokens int
}

// NewBudget makes a budget with the given ceiling; limit <= 0 returns nil (no
// budget), so callers can pass the flag value straight through.
func NewBudget(limit float64) *Budget {
	if limit <= 0 {
		return nil
	}
	return &Budget{limit: limit}
}

// Note accumulates one finished turn. Falls back to the legacy Reply.CostUSD
// when the backend filled cost but not the Usage struct (older serve peers).
func (b *Budget) Note(r Reply) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Usage != nil {
		if r.Usage.CostSource == CostReal {
			b.usd += r.Usage.CostUSD
		} else {
			b.tokens += r.Usage.TotalTokens()
		}
		return
	}
	b.usd += r.CostUSD
}

// Allow reports whether ANOTHER turn may start. Nil budget always allows.
func (b *Budget) Allow() bool { return !b.Exceeded() }

// Exceeded reports whether either meter has crossed the ceiling.
func (b *Budget) Exceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usd > b.limit || float64(b.tokens) > b.limit
}

// String renders the meters for the refusal message ("spent $0.41 + 12894
// tokens of --budget 5000").
func (b *Budget) String() string {
	if b == nil {
		return "(no budget)"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("spent $%.4f + %d tokens of --budget %g", b.usd, b.tokens, b.limit)
}
