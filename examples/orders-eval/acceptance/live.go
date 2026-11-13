package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/goadesign/surla/examples/orders-eval/harness"
	"github.com/goadesign/surla/examples/orders-eval/sim"
	"github.com/goadesign/surla/verify/suite"
)

// Live starts the pinned orders platform as the live copy Surla's
// start_live_target runs, the way a scenario starts it: its own simulator
// loaded with the scenarios' fixtures, a Temporal dev server, the refund
// worker, and the orders API planning with the evaluator's scripted model
// through the harness's model gateway. key is the live key Surla minted; it
// reaches only the orders API, through its environment, so Surla's
// draft_override can change the refund agent's prompts in this copy alone.
// The copy stops when ctx ends or when a candidate process exits, and Done
// then reports the cleanup.
func Live(ctx context.Context, env *harness.Env, key string) (suite.LiveTarget, error) {
	s := sim.New()
	depsURL, err := s.Serve(ctx)
	if err != nil {
		return suite.LiveTarget{}, fmt.Errorf("serve simulator: %w", err)
	}
	s.Reset(fixtures()...)
	p, err := env.StartLive(ctx, depsURL, key)
	if err != nil {
		return suite.LiveTarget{}, errors.Join(fmt.Errorf("start candidate: %w", err), stopSim(s))
	}
	exited := p.Exited()
	done := make(chan error, 1)
	go func() {
		var cause error
		select {
		case <-ctx.Done():
		case <-exited:
			cause = fmt.Errorf("a candidate process exited\n%s", p.Logs())
		}
		// Canceling ctx also ends the candidate processes, so when ctx is
		// canceled the ending is a requested stop whichever case was ready
		// first.
		if ctx.Err() != nil {
			cause = nil
		}
		done <- errors.Join(cause, p.Close(), stopSim(s))
	}()
	return suite.LiveTarget{URL: p.URL(), Done: done}, nil
}

// stopSim stops the simulator, waiting at most simStopTimeout for requests
// it already accepted.
func stopSim(s *sim.Sim) error {
	ctx, cancel := context.WithTimeout(context.Background(), simStopTimeout)
	defer cancel()
	return s.Stop(ctx)
}
