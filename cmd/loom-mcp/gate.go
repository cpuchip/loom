package main

// The tap gate — loom-mcp's ONLY reach into the substrate Postgres, and it is
// narrow by design: enqueue a tool-confirm row (the tap card), read that row's
// status, and — the safe direction only — decline (withdraw) a pending row.
//
// Verified against the live extension contract (v17-gates-worldchat.sql,
// v09-routing-and-hinge.sql):
//   - Enqueue:  stewards.tool_confirm_gate(tool text, args jsonb, target jsonb,
//               agent text, session text) → jsonb {hinge_id, ...}. The card is a
//               row in stewards.hinge_reviews with kind='tool-confirm'.
//   - Status:   the row's `status` — 'pending'/'escalated' while it waits,
//               'applied' once Michael approves (approve runs the stored call and
//               marks it applied), 'declined' if declined.
//   - Verdict:  stewards.tool_confirm_verdict(id, 'approve'|'decline', reason,
//               reviewer). Because 'tool-confirm' is in hinge_escalate_always_kinds,
//               only reviewer='michael' can make an APPROVE stick (any other
//               reviewer's approve escalates and never executes). loom-mcp
//               therefore NEVER approves — approval is Michael's tap alone. It may
//               DECLINE its own pending row to withdraw a commission (a decline
//               never executes anything).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type pgGate struct {
	pool *pgxpool.Pool
}

func newPGGate(ctx context.Context, dsn string) (*pgGate, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect substrate pg: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping substrate pg: %w", err)
	}
	return &pgGate{pool: pool}, nil
}

func (g *pgGate) close() {
	if g.pool != nil {
		g.pool.Close()
	}
}

// enqueue creates the tap card. target is '{}' — the action is executed by
// loom-mcp on approval (it polls status), not by the substrate, so the gate needs
// no executable target. Returns the hinge id to poll.
func (g *pgGate) enqueue(ctx context.Context, tool, agent, session string, args map[string]any) (int64, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return 0, err
	}
	var raw string
	err = g.pool.QueryRow(ctx,
		`SELECT stewards.tool_confirm_gate($1, $2::jsonb, '{}'::jsonb, $3, $4)::text`,
		tool, string(argsJSON), agent, session).Scan(&raw)
	if err != nil {
		return 0, fmt.Errorf("tool_confirm_gate: %w", err)
	}
	var env struct {
		HingeID int64 `json:"hinge_id"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return 0, fmt.Errorf("parse gate envelope %q: %w", raw, err)
	}
	if env.HingeID == 0 {
		return 0, fmt.Errorf("gate returned no hinge_id: %s", raw)
	}
	return env.HingeID, nil
}

// status reads the gate row's lifecycle status.
func (g *pgGate) status(ctx context.Context, hingeID int64) (string, error) {
	var st string
	err := g.pool.QueryRow(ctx,
		`SELECT status FROM stewards.hinge_reviews WHERE id = $1`, hingeID).Scan(&st)
	if err != nil {
		return "", err
	}
	return st, nil
}

// withdraw declines a pending gate row (the safe direction — never executes). Used
// to clear a card when a pending commission is session_closed or times out.
func (g *pgGate) withdraw(ctx context.Context, hingeID int64, reason string) error {
	_, err := g.pool.Exec(ctx,
		`SELECT stewards.tool_confirm_verdict($1::bigint, 'decline', $2, 'loom-mcp')`,
		hingeID, reason)
	return err
}
