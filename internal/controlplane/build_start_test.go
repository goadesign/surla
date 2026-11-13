// This file checks build admission outcomes without real agents or window effects.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"
)

// TestBuildRefusalSpansKeepTruthfulStatus checks the request and build consumers:
// declared structured refusals stay expected; actual errors retain error status.
func TestBuildRefusalSpansKeepTruthfulStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code codes.Code
	}{
		{name: "structured refusal", err: &gensurla.BuildStartRefusal{Name: "working_copy_busy", Step: "Start again after the check ends."}, code: codes.Unset},
		{name: "ordinary refusal", err: gensurla.MakeConflict(errors.New("revision changed")), code: codes.Unset},
		{name: "fault", err: goa.Fault("store failed"), code: codes.Error},
		{name: "unexpected failure", err: errors.New("store failed"), code: codes.Error},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
			tracer := provider.Tracer("test")
			next := func(ctx context.Context, _ any) (any, error) {
				_, span := tracer.Start(ctx, "surla.controlplane.start_build")
				defer span.End()
				return nil, recordBuildError(span, tc.err, "start build")
			}
			_, err := authorizeOperation("start_build", next, tracer)(asCaller("alice@example.com", CallerUser), nil)
			assert.ErrorIs(t, err, tc.err)
			spans := exporter.GetSpans()
			require.Len(t, spans, 2)
			for _, span := range spans {
				assert.Equal(t, tc.code, span.Status.Code, span.Name)
			}
		})
	}
}

// TestBuildStartAdmissionOwnsCancellation keeps accepted work after a client
// disconnect and makes Close cancel and drain it before stores are released.
func TestBuildStartAdmissionOwnsCancellation(t *testing.T) {
	cp := newControlPlane(t, Options{})
	request, disconnect := context.WithCancel(asCaller("alice@example.com", CallerUser))
	accepted, finish, err := cp.admitBuildStart(request)
	require.NoError(t, err)
	finishStart := sync.OnceFunc(finish)
	defer finishStart()
	disconnect()
	assert.NoError(t, accepted.Err())
	principal, ok := PrincipalFromContext(accepted)
	assert.True(t, ok)
	assert.Equal(t, "alice@example.com", principal.ID)
	phase, cancelPhase := cp.buildStartDeadline(accepted)
	defer cancelPhase()
	deadline, ok := phase.Deadline()
	assert.True(t, ok)
	assert.InDelta(t, defaultBuildStartTimeout.Seconds(), time.Until(deadline).Seconds(), 1)

	closed := make(chan error, 1)
	go func() { closed <- cp.Close() }()
	select {
	case <-accepted.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel accepted work")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close finished before accepted work drained: %v", err)
	default:
	}
	finishStart()
	select {
	case err := <-closed:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after accepted work drained")
	}
	_, _, err = cp.admitBuildStart(request)
	assertRefused(t, err, "unavailable")
}

// TestBuildStartLimitUsesTheDeadlineSeam expires accepted work with a short
// limit while retaining the one production default of five minutes.
func TestBuildStartLimitUsesTheDeadlineSeam(t *testing.T) {
	cp := newControlPlane(t, Options{})
	cp.buildStartTimeout = time.Millisecond
	accepted, finish, err := cp.admitBuildStart(t.Context())
	require.NoError(t, err)
	defer finish()
	phase, cancel := cp.buildStartDeadline(accepted)
	defer cancel()
	select {
	case <-phase.Done():
		assert.ErrorIs(t, phase.Err(), context.DeadlineExceeded)
		assert.NoError(t, accepted.Err(), "the team phase retains the server-owned lifetime")
	case <-time.After(time.Second):
		t.Fatal("accepted start did not reach its limit")
	}
}

// TestBuildTeamOutcomeUsesTypedConditions keeps peer-authored messages out of
// every warning and returns the original team result when admission succeeds.
func TestBuildTeamOutcomeUsesTypedConditions(t *testing.T) {
	team := &gensurla.ProjectTeam{Team: "team", Space: "space", Dialog: "not_needed", Roles: []*gensurla.ProjectTeamRole{}}
	private := "private external reply"
	cases := []struct {
		name string
		err  error
		kind gensurla.TeamKind
	}{
		{name: "ready", kind: gensurla.TeamKindReady},
		{name: "window busy", err: gensurla.MakeWindowBusy(errors.New(private)), kind: gensurla.TeamKindWindowBusy},
		{name: "ambiguous", err: &gensurla.TeamSpaceConflict{Name: "ambiguous_space", Count: 3}, kind: gensurla.TeamKindAmbiguousSpace},
		{name: "unavailable", err: gensurla.MakeTeamUnavailable(errors.New(private)), kind: gensurla.TeamKindUnavailable},
		{name: "unexpected failure", err: errors.New(private), kind: gensurla.TeamKindUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := buildTeamOutcome(team, tc.err)
			assert.Equal(t, tc.kind, outcome.Kind())
			if ready, ok := outcome.AsReady(); ok {
				assert.Same(t, team, ready)
			}
			var step gensurla.BuildStartStep
			if busy, ok := outcome.AsWindowBusy(); ok {
				step = busy.Step
			}
			if ambiguous, ok := outcome.AsAmbiguousSpace(); ok {
				assert.Equal(t, 3, ambiguous.Count)
				step = ambiguous.Step
			}
			if unavailable, ok := outcome.AsUnavailable(); ok {
				step = unavailable.Step
			}
			if tc.err != nil {
				assert.True(t, boundedText(string(step), maxCheckStep))
				assert.NotContains(t, string(step), private)
			}
		})
	}
}

// TestStartedBuildDerivesRequiredTestCount reports finished flows from the saved
// Story, including a build that requires no Story test, without a saved count.
func TestStartedBuildDerivesRequiredTestCount(t *testing.T) {
	cases := []struct {
		name    string
		stories []designeval.StorySummary
		count   int
	}{
		{name: "no Story"},
		{name: "unfinished only", stories: []designeval.StorySummary{{Flows: []designeval.StoryFlow{{Key: "draft"}}}}},
		{name: "finished and unfinished", stories: refundStory("Refund over the limit"), count: 1},
		{name: "several Stories", stories: append(refundStory("Refund over the limit"), refundStory("Refund with a receipt")...), count: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := startedBuild(heldBuild{file: buildFile{
				Stories: tc.stories, TestWriter: testWriterAssignment{State: testsAssigned},
			}})
			assert.Equal(t, tc.count, build.RequiredTestCount)
		})
	}
	raw, err := json.Marshal(buildFile{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "required_test_count")
}
