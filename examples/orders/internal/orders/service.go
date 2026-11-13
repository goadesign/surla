package orders

import (
	"context"
	"errors"
	"fmt"

	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
)

type (
	// Service implements the generated orders service.
	Service struct {
		// orders reads customer orders from the commerce system.
		orders OrderSource
		// refunds runs refunds exactly once.
		refunds RefundRunner
		// conversations runs the refund agent.
		conversations Conversations
		// signingKey verifies HS256 bearer tokens.
		signingKey []byte
	}

	// Conversations runs refund agent conversations. The agent wiring in
	// package assistant implements it on top of the Goa-AI runtime.
	Conversations interface {
		// Start starts a run that answers the customer's message.
		Start(ctx context.Context, tenantID, customerID, message string) (RunState, error)
		// Status returns the current state of a run. It fails with
		// ErrRunNotFound when the run does not exist.
		Status(ctx context.Context, runID string) (RunState, error)
		// Decide continues a run waiting for approval with the manager's
		// decision and returns the continued run's state.
		Decide(ctx context.Context, run RunState, approved bool, decidedBy string) (RunState, error)
	}

	// RunState is the state of one refund agent run.
	RunState struct {
		// RunID identifies the run.
		RunID string
		// TenantID is the tenant whose customer started the run.
		TenantID string
		// CustomerID is the customer who started the run.
		CustomerID string
		// Status is "running", "awaiting_approval", "completed" or "failed".
		Status string
		// Reply is the assistant's answer once the run completed.
		Reply string
		// ApprovalID identifies the pending approval when Status is
		// "awaiting_approval".
		ApprovalID string
		// ApprovalPrompt describes what the manager is asked to approve.
		ApprovalPrompt string
	}
)

const (
	// ordersPageSize is how many orders one list_orders page returns.
	ordersPageSize = 10
	// statusAwaitingApproval marks a run suspended until a manager decides.
	statusAwaitingApproval = "awaiting_approval"
)

var (
	// ErrRunNotFound reports a run that does not exist.
	ErrRunNotFound = errors.New("run not found")
)

// New returns the orders service. signingKey verifies the HS256 bearer tokens
// issued by the merchant's identity provider.
func New(orders OrderSource, refunds RefundRunner, conversations Conversations, signingKey []byte) *Service {
	return &Service{orders: orders, refunds: refunds, conversations: conversations, signingKey: signingKey}
}

// Chat starts a refund agent run for the authenticated customer.
func (s *Service) Chat(ctx context.Context, p *genorders.ChatPayload) (*genorders.RunView, error) {
	caller := principal(ctx)
	run, err := s.conversations.Start(ctx, caller.TenantID, caller.Subject, p.Message)
	if err != nil {
		return nil, fmt.Errorf("start conversation: %w", err)
	}
	return runView(run), nil
}

// RunStatus returns a run the authenticated customer started. Runs of other
// customers are reported as not found so their existence is not revealed.
func (s *Service) RunStatus(ctx context.Context, p *genorders.RunStatusPayload) (*genorders.RunView, error) {
	caller := principal(ctx)
	run, err := s.loadRun(ctx, p.RunID)
	if err != nil {
		return nil, err
	}
	if run.TenantID != caller.TenantID || run.CustomerID != caller.Subject {
		return nil, genorders.MakeNotFound(fmt.Errorf("run %s not found", p.RunID))
	}
	return runView(run), nil
}

// ApproveRefund records a manager's decision on a refund waiting for approval.
// The generated endpoint has already required the refunds:approve scope; this
// method additionally requires the run to belong to the manager's tenant and
// the approval to be the one currently pending.
func (s *Service) ApproveRefund(ctx context.Context, p *genorders.ApproveRefundPayload) (*genorders.RunView, error) {
	caller := principal(ctx)
	run, err := s.loadRun(ctx, p.RunID)
	if err != nil {
		return nil, err
	}
	if run.TenantID != caller.TenantID {
		return nil, genorders.MakeNotFound(fmt.Errorf("run %s not found", p.RunID))
	}
	if run.Status != statusAwaitingApproval || run.ApprovalID != p.ApprovalID {
		return nil, genorders.MakeNotFound(fmt.Errorf("approval %s is not pending on run %s", p.ApprovalID, p.RunID))
	}
	next, err := s.conversations.Decide(ctx, run, p.Approved, caller.Subject)
	if err != nil {
		return nil, fmt.Errorf("decide approval %s: %w", p.ApprovalID, err)
	}
	return runView(next), nil
}

// runView converts a run state into the API's run view.
func runView(r RunState) *genorders.RunView {
	v := &genorders.RunView{RunID: r.RunID, Status: r.Status}
	if r.Reply != "" {
		v.Reply = &r.Reply
	}
	if r.ApprovalID != "" {
		v.ApprovalID = &r.ApprovalID
		v.ApprovalPrompt = &r.ApprovalPrompt
	}
	return v
}

// loadRun loads a run and maps a missing run to the not_found error.
func (s *Service) loadRun(ctx context.Context, runID string) (RunState, error) {
	run, err := s.conversations.Status(ctx, runID)
	if errors.Is(err, ErrRunNotFound) {
		return RunState{}, genorders.MakeNotFound(fmt.Errorf("run %s not found", runID))
	}
	if err != nil {
		return RunState{}, fmt.Errorf("load run %s: %w", runID, err)
	}
	return run, nil
}
