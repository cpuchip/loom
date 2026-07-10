package loom

import (
	"context"
	"fmt"
	"strings"
)

// duo — a two-agent build loop bound to one working directory: a WORKER that builds and a
// CRITIC that evaluates the worker's trajectory at every build point, with loom running
// the loop between them. The build point is the TURN BOUNDARY (loom's per-turn exec model
// already gives a natural checkpoint after each reply), so the loop is simply: worker turn
// → critic turn → route on the critic's verdict. The two seats have OPPOSED mandates — the
// worker is driven to finish, the critic to distrust the finish — and the critic inspects
// reality itself (the files, the diffs) rather than trusting the worker's report. That
// opposition is the whole point: a lone agent grading its own work has no one to catch the
// gap between "I built it" and "it works." Nothing outranks the check — a worker declaring
// BUILD COMPLETE does not end the loop; only a critic DONE (or the rounds cap) does.

// DuoDefaultRounds caps the build-point loop when the caller passes Rounds <= 0 (and is the
// CLI's default). Six is enough for a real increment→review→revise arc without letting a
// stuck pair spin forever.
const DuoDefaultRounds = 6

// DuoBuildComplete is the line a worker begins its reply with when it believes the whole
// task is done. It is ADVISORY only — loom never ends the loop on it; the critic still
// judges that final report (nothing outranks the check).
const DuoBuildComplete = "BUILD COMPLETE"

// duo verdicts — the critic ends its reply with one of these on a `VERDICT:` line.
const (
	verdictContinue = "CONTINUE"
	verdictRevise   = "REVISE"
	verdictDone     = "DONE"
)

// duo run statuses (the JSON `status` field).
const (
	duoStatusDone      = "done"
	duoStatusExhausted = "rounds_exhausted"
)

// The routing messages loom sends into the WORKER session between rounds. REVISE prefixes
// the critic's feedback with duoReviseLead; CONTINUE sends duoContinueMsg verbatim.
const (
	duoReviseLead  = "Critic feedback on your last build point — address it before proceeding:"
	duoContinueMsg = "Proceed to the next build point."
)

// DuoConfig is one duo run. Worker and Critic may be the SAME backend — the critic defaults
// to the worker's seat when Critic is nil and to the worker's model when CriticOpts.Model
// is empty. WorkerOpts carries the caller's trust flags exactly as `run` does; CriticOpts
// should share the worker's Workdir (so the critic inspects the same tree) — Duo forces
// CriticOpts.Consult regardless, so the critic can never be opened as a writing seat.
type DuoConfig struct {
	Worker     Backend
	Critic     Backend // nil → defaults to Worker
	WorkerOpts SessionOpts
	CriticOpts SessionOpts // Consult forced true by Duo; Model defaults to WorkerOpts.Model
	Task       string
	Rounds     int // <= 0 → DuoDefaultRounds
	Observer   DuoObserver
}

// DuoRound is the critic's judgment at one build point — what the JSON output carries per
// round. FeedbackSummary is a one-line clip of the critic's feedback (the FULL feedback is
// what gets routed into the worker; the summary is for the human / the log).
type DuoRound struct {
	Verdict         string `json:"verdict"`
	FeedbackSummary string `json:"feedback_summary"`
}

// DuoResult is the outcome of a duo run. Text is the worker's final build-point report (the
// report the critic judged DONE, or the last one before the rounds cap). CostUSD sums both
// seats across all rounds where the backends report cost (0 otherwise). The two session ids
// surface so either seat can be resumed / inspected afterward with `loom run --resume`.
type DuoResult struct {
	WorkerSession string     `json:"worker_session"`
	CriticSession string     `json:"critic_session"`
	Rounds        []DuoRound `json:"rounds"`
	Status        string     `json:"status"`
	Text          string     `json:"text"`
	CostUSD       float64    `json:"cost_usd"`
}

// DuoObserver receives progress callbacks during a duo run so a caller (the CLI) can narrate
// round banners + verdicts and stream each seat's tool events, without Duo itself knowing
// about stderr or flags. Every field is optional (nil-safe). Round fires BEFORE the worker
// turn so streamed events land under the right banner; WorkerEvent / CriticEvent carry each
// seat's tool calls when the caller wants `-events`.
type DuoObserver struct {
	Round       func(round int)
	WorkerReply func(round int, text string)
	Verdict     func(round int, verdict, feedback string)
	Warn        func(msg string)
	WorkerEvent func(ev Event)
	CriticEvent func(ev Event)
}

func (o DuoObserver) round(n int) {
	if o.Round != nil {
		o.Round(n)
	}
}
func (o DuoObserver) workerReply(n int, text string) {
	if o.WorkerReply != nil {
		o.WorkerReply(n, text)
	}
}
func (o DuoObserver) verdict(n int, v, f string) {
	if o.Verdict != nil {
		o.Verdict(n, v, f)
	}
}
func (o DuoObserver) warn(msg string) {
	if o.Warn != nil {
		o.Warn(msg)
	}
}

// WorkerReportedComplete reports whether a worker reply opens with the BUILD COMPLETE
// sentinel (leading whitespace tolerated). Advisory only — see DuoBuildComplete.
func WorkerReportedComplete(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), DuoBuildComplete)
}

// Duo runs the worker↔critic build loop. It opens two sessions on the SAME working
// directory: the worker with the caller's trust flags, and the critic ALWAYS read-only
// (Consult forced true — the critic answers, it never acts). Each round is worker turn →
// critic turn → route: REVISE sends the critic's feedback back into the worker; CONTINUE
// tells the worker to proceed; DONE ends the loop. The critic session is RESUMED each round
// (same Session object), so it accumulates the whole trajectory across rounds — that memory
// is the point. An unparseable verdict fails OPEN — treated as CONTINUE with a warning — so
// a critic that garbles its verdict never wedges the loop; only a real DONE or the rounds
// cap ends it.
func Duo(ctx context.Context, cfg DuoConfig) (res DuoResult, err error) {
	rounds := cfg.Rounds
	if rounds <= 0 {
		rounds = DuoDefaultRounds
	}
	obs := cfg.Observer

	workerSess, err := cfg.Worker.Open(ctx, cfg.WorkerOpts)
	if err != nil {
		return res, fmt.Errorf("duo: open worker: %w", err)
	}
	defer workerSess.Close()

	// The critic seat/model default to the worker's when the caller omitted them, and the
	// critic is ALWAYS read-only: force Consult here so the guarantee holds no matter how
	// CriticOpts was built. Consult is instruction-level (see SessionOpts) — a caller
	// wanting a hard wall pairs it with AllowedTools; here we guarantee the seat is at least
	// *told* to answer-don't-act.
	critic := cfg.Critic
	if critic == nil {
		critic = cfg.Worker
	}
	criticOpts := cfg.CriticOpts
	if criticOpts.Model == "" {
		criticOpts.Model = cfg.WorkerOpts.Model
	}
	criticOpts.Consult = true
	criticSess, err := critic.Open(ctx, criticOpts)
	if err != nil {
		return res, fmt.Errorf("duo: open critic: %w", err)
	}
	defer criticSess.Close()

	// Capture the session ids on EVERY exit path. They're only known after the first turn
	// for some backends, so read them at return (via defer), not at open. Registered only
	// after both opens succeed, so an open error can't nil-deref here.
	defer func() {
		res.WorkerSession = workerSess.SessionID()
		res.CriticSession = criticSess.SessionID()
	}()

	res.Status = duoStatusExhausted
	workerPrompt := cfg.Task + "\n\n" + duoWorkerPreamble

	for round := 1; round <= rounds; round++ {
		obs.round(round)

		wReply, werr := duoSend(ctx, workerSess, workerPrompt, obs.WorkerEvent)
		if werr != nil {
			return res, fmt.Errorf("duo: worker turn %d: %w", round, werr)
		}
		res.CostUSD += wReply.CostUSD
		res.Text = wReply.Text
		obs.workerReply(round, wReply.Text)

		// Round 1 hands the critic its role + the original task + the report. Later rounds
		// hand it ONLY the latest report — the critic session is resumed, so role and task
		// are already in its context; re-sending them would just crowd the trajectory it is
		// meant to accumulate.
		var criticPrompt string
		if round == 1 {
			criticPrompt = fmt.Sprintf(duoCriticFirst, cfg.Task, wReply.Text)
		} else {
			criticPrompt = fmt.Sprintf(duoCriticNext, wReply.Text)
		}
		cReply, cerr := duoSend(ctx, criticSess, criticPrompt, obs.CriticEvent)
		if cerr != nil {
			return res, fmt.Errorf("duo: critic turn %d: %w", round, cerr)
		}
		res.CostUSD += cReply.CostUSD

		verdict, feedback, ok := parseVerdict(cReply.Text)
		if !ok {
			obs.warn(fmt.Sprintf("round %d: critic gave no parseable VERDICT line — treating as CONTINUE", round))
			verdict, feedback = verdictContinue, "" // fail open on the loop; don't route garbage as feedback
		}
		obs.verdict(round, verdict, feedback)
		res.Rounds = append(res.Rounds, DuoRound{Verdict: verdict, FeedbackSummary: duoSummarize(feedback)})

		if verdict == verdictDone {
			res.Status = duoStatusDone
			return res, nil
		}
		if verdict == verdictRevise {
			workerPrompt = duoReviseLead + "\n\n" + feedback
		} else {
			workerPrompt = duoContinueMsg
		}
	}
	return res, nil // loop exhausted — res.Status stays rounds_exhausted, res.Text is the last report
}

// duoSend runs one turn, streaming the seat's tool events to onEvent when the caller wants
// them (nil → the plain final-text path). Mirrors sendTurn's Send/SendStream split.
func duoSend(ctx context.Context, sess Session, prompt string, onEvent func(Event)) (Reply, error) {
	if onEvent != nil {
		return sess.SendStream(ctx, prompt, onEvent)
	}
	return sess.Send(ctx, prompt)
}

// parseVerdict scans the critic's reply for its trailing verdict line and returns the
// verdict plus the feedback above it. ok is false when no recognizable `VERDICT:` line is
// present — the caller fails open (CONTINUE) rather than stalling the loop. Simple line
// scan, no regexp: we take the LAST matching line so a verdict mentioned mid-feedback
// ("don't say VERDICT: DONE yet") never beats the real trailing one, and everything above
// that line is the feedback.
func parseVerdict(text string) (verdict, feedback string, ok bool) {
	lines := strings.Split(text, "\n")
	idx := -1
	for i, ln := range lines {
		t := strings.ToUpper(strings.TrimSpace(ln))
		if !strings.HasPrefix(t, "VERDICT:") {
			continue
		}
		token := strings.TrimSpace(strings.TrimPrefix(t, "VERDICT:"))
		switch {
		case strings.HasPrefix(token, verdictContinue):
			verdict, idx = verdictContinue, i
		case strings.HasPrefix(token, verdictRevise):
			verdict, idx = verdictRevise, i
		case strings.HasPrefix(token, verdictDone):
			verdict, idx = verdictDone, i
		}
	}
	if idx == -1 {
		return "", strings.TrimSpace(text), false
	}
	return verdict, strings.TrimSpace(strings.Join(lines[:idx], "\n")), true
}

// duoSummarize clips feedback to a single line for the per-round JSON record. Rune-aware so
// a multibyte character is never split at the cap.
func duoSummarize(feedback string) string {
	const max = 200
	for _, ln := range strings.Split(feedback, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			if r := []rune(ln); len(r) > max {
				return string(r[:max]) + "…"
			}
			return ln
		}
	}
	return ""
}

// The duo prompts. loom (not the caller) appends duoWorkerPreamble to the task and frames
// each critic turn — the loop's behavior depends on the worker reporting build points and
// the critic ending with a parseable VERDICT line, so those contracts live here in code,
// not in the caller's prompt.

// duoWorkerPreamble is appended after the caller's task (task + "\n\n" + preamble).
const duoWorkerPreamble = `You are the WORKER in a loom duo — a two-agent build loop. You do the building; a separate critic reviews your trajectory at every build point and has READ-ONLY access to this same working directory.

Work in coherent increments. End every reply with a build-point report covering three things: (1) what you did this increment, (2) what you actually VERIFIED — the build, test, or command you ran and its real result, not what you expect — and (3) what is next. Between your turns the critic inspects your real changes and replies with feedback or a verdict; when you receive feedback, address it before proceeding.

When the ENTIRE task is complete, begin your reply with this exact line:

BUILD COMPLETE

on its own line, followed by your final report. Reporting BUILD COMPLETE does not end the review — the critic still judges the finished work, so be sure it is actually done and verified before you say it.`

// duoCriticFirst frames the critic's FIRST turn: role + the original task (%s) + the
// worker's first report (%s).
const duoCriticFirst = `You are the TRAJECTORY CRITIC in a loom duo. A worker is building toward a task and you evaluate its PROCESS at each build point. You are not grading the worker's report — you are checking reality against it. You have READ-ONLY access to the SAME working directory the worker is building in: read the files, run read-only checks (git diff, git status, the tests, the build), and judge what is ACTUALLY there. Where the report and the tree disagree, the tree wins.

THE TASK THE WORKER WAS GIVEN:
%s

THE WORKER'S BUILD-POINT REPORT:
%s

Evaluate this build point: is the work on track, correct, and actually verified? Give concrete, actionable feedback — what is wrong, what is missing, what still needs proving. Then end your reply with EXACTLY ONE of these lines, and make it the LAST line:

VERDICT: CONTINUE   (on track — proceed to the next build point)
VERDICT: REVISE     (fix what you named before proceeding — your feedback is sent to the worker)
VERDICT: DONE       (the task is fully and correctly complete — end the loop)`

// duoCriticNext frames every LATER critic turn: just the worker's latest report (%s). The
// critic session is resumed, so its role, the task, and the trajectory so far are already
// in context.
const duoCriticNext = `THE WORKER'S NEXT BUILD-POINT REPORT:
%s

Evaluate this build point against the task and the trajectory so far. Inspect the working directory yourself — do not trust the report where you can check the tree. Give concrete feedback, then end with EXACTLY ONE of these as the LAST line:

VERDICT: CONTINUE
VERDICT: REVISE
VERDICT: DONE`
