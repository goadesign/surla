package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// TestWritePreviewResultKeepsJSONOnStdout verifies surla preview's output
// contract: stdout holds only the indented JSON result, which decodes back to
// the same value with nothing after it, and stderr holds one summary line,
// which stays one line even when the system name contains a newline.
func TestWritePreviewResultKeepsJSONOnStdout(t *testing.T) {
	cases := []struct {
		name       string
		systemName string
		wantStderr string
	}{
		{
			name:       "plain name",
			systemName: "orders",
			wantStderr: "surla: \"orders\": 1 container(s), 0 boundary edge(s), 0 agent policy(s), 1 tool contract(s)\n",
		},
		{
			name:       "name with a newline",
			systemName: "orders\nsurla: forged",
			wantStderr: "surla: \"orders\\nsurla: forged\": 1 container(s), 0 boundary edge(s), 0 agent policy(s), 1 tool contract(s)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := designeval.Result{
				SystemName:   tc.systemName,
				Declarations: map[string]string{},
				Containers:   []*gensurla.C4ContainerItem{{ID: "orders"}},
				Tools:        []*gensurla.ToolContractItem{{Name: "price"}},
			}
			var stdout, stderr bytes.Buffer
			require.NoError(t, writePreviewResult(&stdout, &stderr, res))

			dec := json.NewDecoder(&stdout)
			var got designeval.Result
			require.NoError(t, dec.Decode(&got))
			assert.Equal(t, res, got)
			var extra json.RawMessage
			assert.ErrorIs(t, dec.Decode(&extra), io.EOF, "stdout holds nothing after the JSON")
			assert.Equal(t, tc.wantStderr, stderr.String())
		})
	}
}
