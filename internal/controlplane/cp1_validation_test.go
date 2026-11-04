package controlplane

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestValidateAmbiguityAnswersReportsEveryProblemOnce verifies that
// confirm_intent's answer check accepts exact option text and otherwise
// returns one error naming every missing, invalid and unknown answer, with
// each question's options quoted. The revise_change pointer ends the error
// only when an answer is missing or is not one of its question's options.
func TestValidateAmbiguityAnswersReportsEveryProblemOnce(t *testing.T) {
	questions := []*gensurla.AmbiguityQuestion{
		{ID: "chg-ambiguity-01", Question: "Who signs in?", Options: []string{"Bearer token auth", "Demo persona header"}},
		{ID: "chg-ambiguity-02", Question: "Who accepts offers?", Options: []string{"Listing seller", "Any seller"}},
		{ID: "chg-ambiguity-03", Question: "Keep history?", Options: []string{"Yes", "No"}},
	}
	const pointer = "; each answer must be exactly one of its question's options, and a decision that no option expresses goes in revise_change's prompt"
	complete := map[string]string{
		"chg-ambiguity-01": "Demo persona header",
		"chg-ambiguity-02": "Listing seller",
		"chg-ambiguity-03": "No",
	}
	cases := []struct {
		name    string
		answers map[string]string
		want    string
	}{
		{
			name:    "exact options are accepted",
			answers: complete,
		},
		{
			name: "every problem is reported in question order, then unknown IDs sorted",
			answers: map[string]string{
				"chg-ambiguity-01": "Demo persona selector at the edge",
				"chg-ambiguity-03": "  ",
				"chg-ambiguity-99": "Yes",
				"chg-ambiguity-42": "No",
			},
			want: `answer "Demo persona selector at the edge" for ambiguity "chg-ambiguity-01" is not one of its options: ["Bearer token auth" "Demo persona header"]; ` +
				`missing confirmation answer for ambiguity "chg-ambiguity-02" (options: ["Listing seller" "Any seller"]); ` +
				`missing confirmation answer for ambiguity "chg-ambiguity-03" (options: ["Yes" "No"]); ` +
				`unexpected confirmation answer for unknown ambiguity "chg-ambiguity-42"; ` +
				`unexpected confirmation answer for unknown ambiguity "chg-ambiguity-99"` + pointer,
		},
		{
			name: "an option that differs only in spacing is not an option",
			answers: map[string]string{
				"chg-ambiguity-01": "Bearer token auth ",
				"chg-ambiguity-02": "Listing seller",
				"chg-ambiguity-03": "Yes",
			},
			want: `answer "Bearer token auth " for ambiguity "chg-ambiguity-01" is not one of its options: ["Bearer token auth" "Demo persona header"]` + pointer,
		},
		{
			name: "only an unknown question ID gets no revise_change pointer",
			answers: map[string]string{
				"chg-ambiguity-01": "Demo persona header",
				"chg-ambiguity-02": "Listing seller",
				"chg-ambiguity-03": "No",
				"chg-ambiguity-04": "Yes",
			},
			want: `unexpected confirmation answer for unknown ambiguity "chg-ambiguity-04"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAmbiguityAnswers(questions, tc.answers)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}
