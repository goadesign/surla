// These tests exercise the file boundary and bounded history, without models
// or application processes. Every fixture lives in its test's temporary tree.
package traceview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
)

// TestHistoryIsolationAndContinuation preserves a fixed older range while new
// appends remain available on refresh, and never returns another caller's data.
func TestHistoryIsolationAndContinuation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	page, err := s.List("alice", "all", 2, nil)
	require.NoError(t, err)
	assert.NotNil(t, page.Spans)
	assert.Empty(t, page.Spans)
	_, err = os.Stat(filepath.Join(dir, "traces.jsonl"))
	assert.ErrorIs(t, err, os.ErrNotExist, "empty startup must not obstruct record-empty import")
	for i := range 6 {
		owner := "alice"
		if i == 4 {
			owner = "bob"
		}
		require.NoError(t, s.Record(owner, testSpan(i, codes.Error)))
	}
	page, err = s.List("alice", "error", 2, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"operation-5", "operation-3"}, spanNames(page.Spans))
	require.NotNil(t, page.NextCursor)
	assert.True(t, page.HasMore)
	require.NoError(t, s.Record("alice", testSpan(6, codes.Ok)))
	older, err := s.List("alice", "error", 100, page.NextCursor)
	require.NoError(t, err)
	assert.Equal(t, []string{"operation-2", "operation-1", "operation-0"}, spanNames(older.Spans))
	assert.False(t, older.HasMore)
	assert.Nil(t, older.NextCursor)
	refresh, err := s.List("alice", "ok", 100, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"operation-6"}, spanNames(refresh.Spans))
	other, err := s.List("bob", "all", 100, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"operation-4"}, spanNames(other.Spans))
	info, err := os.Stat(filepath.Join(dir, "traces.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	for _, cursor := range []string{"0", "-1", "01", "garbage", "1", strconv.FormatInt(info.Size()+1, 10)} {
		t.Run(cursor, func(t *testing.T) {
			_, err := s.List("alice", "all", 20, &cursor)
			assert.ErrorIs(t, err, ErrCursor)
		})
	}
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	_, err = s.List("alice", "all", 20, nil)
	assert.ErrorIs(t, err, errClosed)
	assert.NoError(t, s.Record("alice", testSpan(7, codes.Ok)), "late spans are discarded after Close")

	reopened, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	refresh, err = reopened.List("alice", "all", 100, nil)
	require.NoError(t, err)
	assert.Len(t, refresh.Spans, 6)
}

// TestSparseHistoryMakesBoundedProgress proves that empty filtered pages do not
// claim full coverage, and each disk window skips at least 192 KiB of history.
func TestSparseHistoryMakesBoundedProgress(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	fixture := testSpan(1, codes.Ok)
	for range 1200 {
		require.NoError(t, s.Record("bob", fixture))
	}
	info, err := s.file.Stat()
	require.NoError(t, err)
	previous := info.Size()
	var cursor *string
	for calls := 0; calls < 10; calls++ {
		page, err := s.List("alice", "error", 100, cursor)
		require.NoError(t, err)
		assert.Empty(t, page.Spans)
		if !page.HasMore {
			assert.Nil(t, page.NextCursor)
			return
		}
		require.NotNil(t, page.NextCursor)
		next, err := strconv.ParseInt(*page.NextCursor, 10, 64)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, previous-next, int64(scanBytes-MaxRecordBytes))
		assert.Less(t, next, previous)
		previous, cursor = next, page.NextCursor
	}
	t.Fatal("bounded test history did not finish within ten disk windows")
}

// TestStoredRecordByteBoundary accepts the inclusive record bound and rejects
// oversized rows, including rows that would otherwise repeat the same cursor.
func TestStoredRecordByteBoundary(t *testing.T) {
	t.Parallel()
	view := SafeSpan(testSpan(1, codes.Error))
	owner := principalHash("alice")
	raw, err := json.Marshal(savedSpan{PrincipalHash: owner, Span: view})
	require.NoError(t, err)
	for _, size := range []int{MaxRecordBytes - 1, MaxRecordBytes, MaxRecordBytes + 1, scanBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			line := string(raw) + strings.Repeat(" ", size-len(raw)-1) + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "traces.jsonl"), []byte(line), 0o600))
			s, err := Open(dir)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, s.Close()) })
			page, err := s.List("alice", "all", 20, nil)
			if size > MaxRecordBytes {
				assert.ErrorContains(t, err, "record size contract")
				assert.Nil(t, page)
			} else {
				require.NoError(t, err)
				assert.Len(t, page.Spans, 1)
				assert.False(t, page.HasMore)
			}
		})
	}
}

// TestSafeRecordBoundsAndOriginalSpan tests worst-case JSON escaping and text
// boundaries while proving original span inputs remain available to exporters.
func TestSafeRecordBoundsAndOriginalSpan(t *testing.T) {
	t.Parallel()
	for _, delta := range []int{-1, 0, 1} {
		t.Run(strconv.Itoa(delta), func(t *testing.T) {
			fixture := testSpan(1, codes.Error)
			stub := tracetest.SpanStubFromReadOnlySpan(fixture)
			stub.Name = strings.Repeat("\x00", nameBytes+delta)
			stub.Attributes = []attribute.KeyValue{
				attribute.String(SummaryAttribute, strings.Repeat("\x00", summaryBytes+delta)),
			}
			for _, key := range []string{"surla.change_id", "surla.run_id", "surla.check.op_id", "surla.check.passed", "surla.generation.target"} {
				stub.Attributes = append(stub.Attributes, attribute.String(key, strings.Repeat("\x00", valueBytes+delta)))
			}
			stub.Attributes = append(stub.Attributes, attribute.String("raw.secret", "/private/operator/key"))
			original := stub.Snapshot()
			view := SafeSpan(original)
			owner := principalHash("alice")
			assert.LessOrEqual(t, len(view.Name), nameBytes)
			assert.LessOrEqual(t, len(view.Summary), summaryBytes)
			assert.Equal(t, delta > 0, view.Shortened)
			assert.Len(t, view.Attributes, 5)
			assert.Equal(t, stub.Name, original.Name())
			assert.Equal(t, stub.Attributes, original.Attributes())
			raw, err := json.Marshal(savedSpan{PrincipalHash: owner, Span: view})
			require.NoError(t, err)
			assert.LessOrEqual(t, len(raw)+1, MaxRecordBytes)
			assert.NotContains(t, string(raw), "raw.secret")
			assert.NotContains(t, string(raw), "/private/operator/key")
			assert.NotContains(t, string(raw), "raw-error-secret")
			assert.NotContains(t, string(raw), "raw-event-secret")
		})
	}
	value, cut := boundedText("猫猫", 5)
	assert.Equal(t, "…", value)
	assert.True(t, cut)
}

// TestRestartRepairsOnlyInterruptedTail preserves complete history exactly and
// validates file type before any repair, including non-blocking pipe rejection.
func TestRestartRepairsOnlyInterruptedTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, s.Record("alice", testSpan(1, codes.Unset)))
	require.NoError(t, s.Close())
	path := filepath.Join(dir, "traces.jsonl")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"partial":`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	restarted, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, restarted.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	for _, kind := range []string{"link", "directory", "pipe"} {
		t.Run(kind, func(t *testing.T) {
			other := t.TempDir()
			victim := filepath.Join(other, "victim")
			require.NoError(t, os.WriteFile(victim, []byte("keep incomplete"), 0o600))
			target := filepath.Join(other, "traces.jsonl")
			switch kind {
			case "link":
				require.NoError(t, os.Symlink(victim, target))
			case "directory":
				require.NoError(t, os.Mkdir(target, 0o700))
			case "pipe":
				require.NoError(t, unix.Mkfifo(target, 0o600))
			}
			_, err := Open(other)
			assert.Error(t, err)
			got, err := os.ReadFile(victim)
			require.NoError(t, err)
			assert.Equal(t, "keep incomplete", string(got))
		})
	}
}

// TestUnknownAndTrailingStoredFieldsReject prevents a malformed saved record
// from silently changing the owner or span returned to an authenticated caller.
func TestUnknownAndTrailingStoredFieldsReject(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	view := SafeSpan(testSpan(1, codes.Error))
	valid, err := json.Marshal(savedSpan{PrincipalHash: principalHash("alice"), Span: view})
	require.NoError(t, err)
	for _, line := range []string{
		string(valid) + `{}`,
		`{"principal_hash":"bad","span":{}}`,
		strings.TrimSuffix(string(valid), "}") + `,"unknown":true}`,
	} {
		_, err := decodeSpan([]byte(line))
		assert.Error(t, err)
	}
	assert.Error(t, s.Record("", testSpan(1, codes.Error)))
	require.NoError(t, s.Close())
	assert.NoError(t, s.Record("alice", testSpan(1, codes.Error)))

}

// TestFailedCheckResultKeepsItsOriginalStatus protects a valid negative result:
// failed tests are not an operation error, and no exporter rewrites that fact.
func TestFailedCheckResultKeepsItsOriginalStatus(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close()) })
	stub := tracetest.SpanStubFromReadOnlySpan(testSpan(1, codes.Unset))
	stub.Name = "surla.controlplane.check_project"
	stub.Attributes = append(stub.Attributes, attribute.Bool("surla.check.passed", false))
	original := stub.Snapshot()
	require.NoError(t, s.Record("alice", original))
	failed, err := s.List("alice", "error", 20, nil)
	require.NoError(t, err)
	assert.Empty(t, failed.Spans)
	completed, err := s.List("alice", "ok", 20, nil)
	require.NoError(t, err)
	require.Len(t, completed.Spans, 1)
	assert.Equal(t, "unset", completed.Spans[0].Status)
	assert.Equal(t, "false", completed.Spans[0].Attributes["surla.check.passed"])
	assert.Equal(t, codes.Unset, original.Status().Code)
}

// testSpan creates immutable SDK-shaped evidence with known raw secrets so
// tests can prove those fields never enter the safe history.
func testSpan(i int, code codes.Code) sdktrace.ReadOnlySpan {
	traceID := trace.TraceID{1}
	spanID := trace.SpanID{1, byte(i >> 8), byte(i)}
	start := time.Date(2026, 1, 2, 3, 4, 5, i, time.UTC)
	return (tracetest.SpanStub{
		Name: fmt.Sprintf("operation-%d", i),
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID,
			SpanID:  spanID,
		}),
		StartTime: start,
		EndTime:   start.Add(time.Millisecond),
		Attributes: []attribute.KeyValue{
			attribute.String("surla.change_id", "change-1"),
		},
		Status: sdktrace.Status{Code: code, Description: "raw-error-secret"},
		Events: []sdktrace.Event{{Name: "raw-event-secret"}},
	}).Snapshot()
}

// spanNames keeps the assertions about chronological history easy to compare.
func spanNames(spans []*gensurla.TraceSpan) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	return names
}
