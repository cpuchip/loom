package loom

import (
	"context"
	"sync"
)

// Panel runs one prompt across multiple backends CONCURRENTLY and collects the
// replies in input order — the "council" pattern (ask N agents the same thing,
// then compare / synthesize). Each backend gets its own fresh session.
//
// A failed backend yields a Reply with Err set rather than aborting the panel,
// so one dead agent never sinks the others.
func Panel(ctx context.Context, backends []Backend, opts SessionOpts, prompt string) []Reply {
	out := make([]Reply, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Add(1)
		go func(i int, b Backend) {
			defer wg.Done()
			r := Reply{Backend: b.Name()}
			sess, err := b.Open(ctx, opts)
			if err != nil {
				r.Err = err.Error()
				out[i] = r
				return
			}
			defer sess.Close()
			rr, err := sess.Send(ctx, prompt)
			rr.Backend = b.Name()
			if err != nil && rr.Err == "" {
				rr.Err = err.Error()
			}
			out[i] = rr
		}(i, b)
	}
	wg.Wait()
	return out
}
