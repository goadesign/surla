package orders

import (
	"context"
	"testing"
	"time"

	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
	"goa.design/goa/v3/security"
)

// fakeConversations serves one run and records decisions.
type fakeConversations struct {
	// run is the only run that exists.
	run RunState
	// decided records the approvals passed to Decide.
	decided []string
}

var testKey = []byte("test-signing-key")

func TestJWTAuth(t *testing.T) {
	approve := &security.JWTScheme{RequiredScopes: []string{"refunds:approve"}}
	cases := []struct {
		name    string
		token   string
		wantErr string
	}{
		{"manager", token(t, testKey, "t1", "m1", "orders:chat refunds:approve", time.Hour), ""},
		{"customer lacks the approval scope", token(t, testKey, "t1", "c1", "orders:chat", time.Hour), "forbidden"},
		{"wrong signing key", token(t, []byte("other"), "t1", "m1", "refunds:approve", time.Hour), "unauthorized"},
		{"expired", token(t, testKey, "t1", "m1", "refunds:approve", -time.Minute), "unauthorized"},
		{"missing tenant", token(t, testKey, "", "m1", "refunds:approve", time.Hour), "unauthorized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := New(nil, nil, nil, testKey)
			ctx, err := svc.JWTAuth(context.Background(), c.token, approve)
			if c.wantErr != "" {
				var se *goa.ServiceError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, c.wantErr, se.Name)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "m1", principal(ctx).Subject)
		})
	}
}

func TestApproveRefund(t *testing.T) {
	pending := RunState{RunID: "run-1", TenantID: "t1", CustomerID: "c1", Status: statusAwaitingApproval, ApprovalID: "ap-1"}
	cases := []struct {
		name       string
		tenantID   string
		approvalID string
		run        RunState
		wantErr    string
	}{
		{"manager of the tenant decides the pending approval", "t1", "ap-1", pending, ""},
		{"manager of another tenant", "t2", "ap-1", pending, "not_found"},
		{"stale approval", "t1", "ap-0", pending, "not_found"},
		{"run no longer waiting", "t1", "ap-1", RunState{RunID: "run-1", TenantID: "t1", Status: "completed"}, "not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conv := &fakeConversations{run: c.run}
			svc := New(nil, nil, conv, testKey)
			ctx := context.WithValue(context.Background(), principalKey{}, Principal{TenantID: c.tenantID, Subject: "m1"})
			_, err := svc.ApproveRefund(ctx, &genorders.ApproveRefundPayload{RunID: "run-1", ApprovalID: c.approvalID, Approved: true})
			if c.wantErr != "" {
				var se *goa.ServiceError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, c.wantErr, se.Name)
				assert.Empty(t, conv.decided)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"ap-1"}, conv.decided)
		})
	}
}

func TestRunStatusHidesOtherCustomersRuns(t *testing.T) {
	conv := &fakeConversations{run: RunState{RunID: "run-1", TenantID: "t1", CustomerID: "c1", Status: "running"}}
	svc := New(nil, nil, conv, testKey)
	ctx := context.WithValue(context.Background(), principalKey{}, Principal{TenantID: "t1", Subject: "c2"})
	_, err := svc.RunStatus(ctx, &genorders.RunStatusPayload{RunID: "run-1"})
	var se *goa.ServiceError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "not_found", se.Name)
}

// token signs an HS256 bearer token with the given claims.
func token(t *testing.T, key []byte, tenantID, subject, scope string, ttl time.Duration) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject, ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
		TenantID:         tenantID,
		Scope:            scope,
	}).SignedString(key)
	require.NoError(t, err)
	return signed
}

// Start is unused by these tests.
func (f *fakeConversations) Start(context.Context, string, string, string) (RunState, error) {
	return RunState{}, nil
}

// Status returns the run or ErrRunNotFound.
func (f *fakeConversations) Status(_ context.Context, runID string) (RunState, error) {
	if runID != f.run.RunID {
		return RunState{}, ErrRunNotFound
	}
	return f.run, nil
}

// Decide records the decided approval and reports the run as running again.
func (f *fakeConversations) Decide(_ context.Context, run RunState, _ bool, _ string) (RunState, error) {
	f.decided = append(f.decided, run.ApprovalID)
	run.Status = "running"
	return run, nil
}
