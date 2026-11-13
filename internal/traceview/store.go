// Package traceview saves safe operation diagnostics for one control plane.
// The control plane holds its cards-directory lock for the store's lifetime.
// Records live only on disk; reads examine one bounded window, newest first.
package traceview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	gensurla "github.com/goadesign/surla/gen/surla"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
)

type (
	// Store owns one append-only file. Open requires the caller to hold the
	// enclosing card-store lock until Close, including during tail recovery.
	Store struct {
		// mu serializes appends, bounded reads, and closing the file.
		mu sync.Mutex
		// file exists only after the first append or opening saved history.
		// No records or decoded history are retained.
		file *os.File
		// path is reserved under the enclosing card lock; no file is created
		// until the first authenticated operation finishes.
		path string
		// closed prevents an absent history from being reopened after Close.
		closed bool
		// writeErr stops appends after a failed write, preserving a recoverable tail.
		writeErr error
	}

	// savedSpan adds private access-control data to the generated API value.
	// The principal hash never leaves this file through List.
	savedSpan struct {
		// PrincipalHash ties the record to the principal present at span start.
		PrincipalHash string `json:"principal_hash"`
		// Span is the safe view, never the original OpenTelemetry span.
		Span *gensurla.TraceSpan `json:"span"`
	}
)

const (
	// SummaryAttribute contains a code-authored safe summary, never a raw error.
	SummaryAttribute = "surla.public_summary"
	// MaxRecordBytes bounds one encoded record, including its final newline.
	MaxRecordBytes = 64 * 1024
	// scanBytes bounds one List disk read; records leave at least 192 KiB of
	// progress when a sparse page has older, unexamined history.
	scanBytes = 256 * 1024
	// nameBytes bounds one operation name in the caller-visible record.
	nameBytes = 128
	// summaryBytes bounds one code-authored safe operation summary.
	summaryBytes = 2048
	// valueBytes bounds each selected attribute value.
	valueBytes = 1024
)

var (
	// ErrCursor means the caller must omit the cursor to refresh its history.
	ErrCursor = errors.New("invalid trace cursor; omit cursor to refresh")
	// errClosed prevents access after the owner releases its directory lock.
	errClosed = errors.New("trace store is closed")
)

// Open opens cardsDir/traces.jsonl under the caller's card-directory lock.
// It creates no file, rejects links and non-private files, and removes only an
// incomplete final append. Complete records remain byte for byte unchanged.
func Open(cardsDir string) (*Store, error) {
	path := filepath.Join(cardsDir, "traces.jsonl")
	f, err := openTraceFile(path, os.O_RDWR|os.O_APPEND)
	if errors.Is(err, os.ErrNotExist) {
		return &Store{path: path}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open trace history: %w", err)
	}
	return &Store{file: f, path: path}, nil
}

// SafeSpan derives a bounded caller view without changing the original span.
// Authentication and ownership belong to the operation that records the view.
func SafeSpan(span sdktrace.ReadOnlySpan) *gensurla.TraceSpan {
	attrs := span.Attributes()
	summary := ""
	values := make(map[string]string)
	shortened := false
	for _, a := range attrs {
		switch string(a.Key) {
		case SummaryAttribute:
			summary = a.Value.AsString()
		default:
			if diagnosticAttribute(string(a.Key)) {
				value, cut := boundedText(a.Value.Emit(), valueBytes)
				values[string(a.Key)] = value
				shortened = shortened || cut
			}
		}
	}
	status := strings.ToLower(span.Status().Code.String())
	if summary == "" {
		if span.Status().Code == codes.Error {
			summary = "Operation failed. Read the tool error for recovery instructions."
		} else {
			summary = "Operation ended; consult its result for the outcome."
		}
	}
	name, nameCut := boundedText(span.Name(), nameBytes)
	summary, summaryCut := boundedText(summary, summaryBytes)
	parent := ""
	if span.Parent().IsValid() {
		parent = span.Parent().SpanID().String()
	}
	return &gensurla.TraceSpan{
		TraceID:      span.SpanContext().TraceID().String(),
		SpanID:       span.SpanContext().SpanID().String(),
		ParentSpanID: parent,
		Name:         name,
		StartTime:    span.StartTime().UTC().Format(time.RFC3339Nano),
		EndTime:      span.EndTime().UTC().Format(time.RFC3339Nano),
		DurationMs:   span.EndTime().Sub(span.StartTime()).Milliseconds(),
		Status:       status,
		Summary:      summary,
		Attributes:   values,
		Shortened:    shortened || nameCut || summaryCut,
	}
}

// Record appends one bounded safe view for the authenticated operation's
// principal and records its trusted caller attribution only in the private
// view. An empty principal is rejected; the original SDK span never changes.
// Calls after Close discard late
// spans without reopening storage after the card-directory lock is released.
func (s *Store) Record(principal, callerID, callerKind string, span sdktrace.ReadOnlySpan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if principal == "" {
		return errors.New("authenticated principal is required to record operation diagnostics")
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	view := SafeSpan(span)
	if callerID != "" {
		view.Attributes["surla.caller_id"] = callerID
		view.Attributes["surla.caller_kind"] = callerKind
	}
	line, err := json.Marshal(savedSpan{PrincipalHash: principalHash(principal), Span: view})
	if err != nil {
		return fmt.Errorf("encode trace history record: %w", err)
	}
	line = append(line, '\n')
	if len(line) > MaxRecordBytes {
		return errors.New("trace history record exceeds its encoded size contract")
	}
	if s.file == nil {
		s.file, err = openTraceFile(s.path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err != nil {
			return fmt.Errorf("create trace history: %w", err)
		}
	}
	if _, err := s.file.Write(line); err != nil {
		s.writeErr = fmt.Errorf("append trace history: %w", err)
		return s.writeErr
	}
	return nil
}

// List reads at most one 256 KiB disk window for principal, newest first.
// Empty matching results still return a cursor when older history remains.
// limit and status have already passed generated API validation.
func (s *Store) List(
	principal, status string,
	limit int,
	cursor *string,
) (*gensurla.TraceList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	if s.file == nil {
		if cursor != nil {
			return nil, ErrCursor
		}
		return &gensurla.TraceList{Spans: []*gensurla.TraceSpan{}}, nil
	}
	info, err := s.file.Stat()
	if err != nil {
		return nil, err
	}
	end := info.Size()
	if cursor != nil {
		end, err = strconv.ParseInt(*cursor, 10, 64)
		if err != nil || end <= 0 || end > info.Size() || strconv.FormatInt(end, 10) != *cursor {
			return nil, ErrCursor
		}
		var last [1]byte
		if _, err := s.file.ReadAt(last[:], end-1); err != nil || last[0] != '\n' {
			return nil, ErrCursor
		}
	}
	start := max(int64(0), end-scanBytes)
	buf := make([]byte, end-start)
	if _, err := s.file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if start > 0 {
		firstEnd := bytes.IndexByte(buf, '\n')
		if firstEnd < 0 || firstEnd >= MaxRecordBytes {
			return nil, errors.New("trace history exceeds its record size contract")
		}
		start += int64(firstEnd + 1)
		buf = buf[firstEnd+1:]
	}
	result := &gensurla.TraceList{Spans: []*gensurla.TraceSpan{}}
	owner := principalHash(principal)
	for len(buf) > 0 {
		if buf[len(buf)-1] != '\n' {
			return nil, errors.New("trace history has an incomplete record")
		}
		previous := bytes.LastIndexByte(buf[:len(buf)-1], '\n')
		lineStart := previous + 1
		line := buf[lineStart : len(buf)-1]
		if len(line)+1 > MaxRecordBytes {
			return nil, errors.New("trace history exceeds its record size contract")
		}
		record, err := decodeSpan(line)
		if err != nil {
			return nil, err
		}
		end = start + int64(lineStart)
		if record.PrincipalHash == owner && matchesStatus(record.Span.Status, status) {
			result.Spans = append(result.Spans, record.Span)
			if len(result.Spans) == limit {
				break
			}
		}
		buf = buf[:lineStart]
	}
	if end > 0 {
		result.HasMore = true
		next := strconv.FormatInt(end, 10)
		result.NextCursor = &next
	}
	return result, nil
}

// Close synchronizes and closes the file before the card owner unlocks its
// directory. Repeated calls are harmless and never reopen the file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		s.closed = true
		return s.writeErr
	}
	f := s.file
	s.file = nil
	s.closed = true
	return errors.Join(s.writeErr, f.Sync(), f.Close())
}

// openTraceFile opens an existing or exclusively created diagnostic file
// under the caller's card-directory lock. It never follows links or blocks on
// pipes and requires a regular file with exactly 0600 permissions. Callers own
// the returned file after any incomplete final append has been removed.
func openTraceFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			err = fmt.Errorf("private diagnostic file %q must not be a symlink; stop Surla and restore the regular file before restarting: %w", path, err)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
		err = fmt.Errorf("private diagnostic file %q must be a regular file with permissions 0600; stop Surla and restore the regular file, or run chmod 600 %q if it is already regular, before restarting", path, path)
	}
	if err == nil {
		err = repairTail(f, info.Size())
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// principalHash keeps authenticated IDs out of the private diagnostic records.
// It is used only while saving or checking file ownership, never on spans.
func principalHash(principal string) string {
	digest := sha256.Sum256([]byte(principal))
	return hex.EncodeToString(digest[:])
}

// repairTail removes only a final interrupted append, using one record window.
func repairTail(f *os.File, size int64) error {
	if size == 0 {
		return nil
	}
	buf := make([]byte, min(size, MaxRecordBytes))
	if _, err := f.ReadAt(buf, size-int64(len(buf))); err != nil {
		return err
	}
	last := bytes.LastIndexByte(buf, '\n')
	if last == len(buf)-1 {
		return nil
	}
	if last < 0 && size > MaxRecordBytes {
		return errors.New("trace history has an oversized incomplete append")
	}
	if err := f.Truncate(size - int64(len(buf)) + int64(last+1)); err != nil {
		return err
	}
	return f.Sync()
}

// decodeSpan validates a saved record at the package's file boundary.
func decodeSpan(line []byte) (savedSpan, error) {
	var record savedSpan
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, fmt.Errorf("decode trace history: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return record, errors.New("trace history record contains trailing data")
	}
	owner, err := hex.DecodeString(record.PrincipalHash)
	if err != nil || len(owner) != sha256.Size || hex.EncodeToString(owner) != record.PrincipalHash ||
		record.Span == nil || record.Span.Attributes == nil {
		return record, errors.New("trace history record is missing required ownership or span data")
	}
	span := record.Span
	if span.Status != "unset" && span.Status != "ok" && span.Status != "error" {
		return record, errors.New("trace history record contains an invalid status")
	}
	if _, err := trace.TraceIDFromHex(span.TraceID); err != nil {
		return record, errors.New("trace history record contains an invalid trace identifier")
	}
	if _, err := trace.SpanIDFromHex(span.SpanID); err != nil {
		return record, errors.New("trace history record contains an invalid span identifier")
	}
	if span.ParentSpanID != "" {
		if _, err := trace.SpanIDFromHex(span.ParentSpanID); err != nil {
			return record, errors.New("trace history record contains an invalid parent identifier")
		}
	}
	for _, timestamp := range []string{span.StartTime, span.EndTime} {
		if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
			return record, errors.New("trace history record contains an invalid timestamp")
		}
	}
	if len(span.Name) > nameBytes || len(span.Summary) > summaryBytes ||
		!utf8.ValidString(span.Name) || !utf8.ValidString(span.Summary) {
		return record, errors.New("trace history record exceeds its safe text bounds")
	}
	for key, value := range span.Attributes {
		if !diagnosticAttribute(key) || len(value) > valueBytes || !utf8.ValidString(value) {
			return record, errors.New("trace history record contains an invalid diagnostic attribute")
		}
	}
	return record, nil
}

// diagnosticAttribute owns the closed set used by both writing and file loads.
func diagnosticAttribute(key string) bool {
	switch key {
	case "surla.change_id", "surla.run_id", "surla.caller_id", "surla.caller_kind",
		"surla.check.op_id",
		"surla.check.passed", "surla.generation.target":
		return true
	default:
		return false
	}
}

// matchesStatus preserves unset as its own fact while supporting an error filter.
func matchesStatus(actual, filter string) bool {
	return filter == "all" || filter == "error" && actual == "error" || filter == "ok" && actual != "error"
}

// boundedText keeps valid UTF-8 and marks any normalization or shortening.
func boundedText(value string, bound int) (string, bool) {
	valid := strings.ToValidUTF8(value, "�")
	if len(valid) <= bound {
		return valid, valid != value
	}
	end := bound - len("…")
	for !utf8.RuneStart(valid[end]) {
		end--
	}
	return valid[:end] + "…", true
}
