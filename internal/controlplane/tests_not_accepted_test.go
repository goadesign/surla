// These tests read the tests_not_accepted refusal directly; no run, repository
// or agent is involved.
package controlplane

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
)

// notAcceptedStates covers each state the test writer's assignment can be in
// while its tests are not accepted.
var notAcceptedStates = []testWriterAssignment{
	{State: testsAssigned},
	{State: testsReported},
	{State: testsNeedChanges, Findings: []string{"refund_over_limit is missing"}},
	{State: testsCouldNotRun, Reason: "git could not write the tests branch", Step: "Free disk space, then report again."},
}

// TestTestsNotAcceptedEndsWithBuilderStep verifies that the refusal keeps each
// state's own first clause and ends, in all four states, with the one step the
// builder can take, which names the acceptance and get_builder_assignment.
func TestTestsNotAcceptedEndsWithBuilderStep(t *testing.T) {
	step := "; your step: keep your Chorus watcher running; Surla messages you there when the tests are accepted; then read get_builder_assignment and verify again"
	firstClauses := map[string]string{
		testsAssigned:    "their state is assigned: the test writer writes the required tests and reports them finished" + step,
		testsReported:    "their state is reported: Surla is checking the test writer's report" + step,
		testsNeedChanges: "their state is needs changes: the test writer fixes Surla's findings and reports again" + step,
		testsCouldNotRun: "their state is could not run: Surla's check could not run (git could not write the tests branch); the user's step: Free disk space, then report again" + step,
	}
	for _, saved := range notAcceptedStates {
		t.Run(saved.State, func(t *testing.T) {
			var refusal *goa.ServiceError
			require.ErrorAs(t, refuseTestsNotAccepted(saved), &refusal)
			assert.Equal(t, "tests_not_accepted", refusal.Name)
			assert.True(t, strings.HasSuffix(refusal.Message, firstClauses[saved.State]), refusal.Message)
			assert.True(t, strings.HasPrefix(refusal.Message, "the tests of this build are not accepted; "), refusal.Message)
		})
	}
}

// TestTestsNotAcceptedNeverSaysTurn keeps the refusal free of any instruction
// about turns: each agent application waits for Chorus in its own way.
func TestTestsNotAcceptedNeverSaysTurn(t *testing.T) {
	turn := regexp.MustCompile(`(?i)\bturns?\b`)
	for _, saved := range notAcceptedStates {
		t.Run(saved.State, func(t *testing.T) {
			message := refuseTestsNotAccepted(saved).Error()
			assert.False(t, turn.MatchString(message), message)
		})
	}
}
