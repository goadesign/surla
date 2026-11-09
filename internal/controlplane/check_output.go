// This file implements read_check_output: bounded pages of one check
// operation's captured output, with an opaque code-owned cursor.
package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// opIDPattern is the shape of an operation ID: 32 lowercase hex digits.
var opIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ReadCheckOutput returns one page of an operation's captured output as at
// most 16 KiB of valid UTF-8 text, starting at the cursor, with the next
// cursor and the operation's captured and dropped byte counts (raw bytes).
// A saved output shorter than its accounting is an error, never an empty
// page. It requires an authenticated principal.
func (c *ControlPlane) ReadCheckOutput(ctx context.Context, p *gensurla.ReadCheckOutputPayload) (res *gensurla.CheckOutputPage, retErr error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	_, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.read_check_output")
	span.SetAttributes(attribute.String("surla.check.op_id", p.OpID))
	defer func() { endCheckSpan(span, retErr) }()
	if !opIDPattern.MatchString(p.OpID) || c.checkWorkDir == "" {
		return nil, gensurla.MakeNotFound(fmt.Errorf("no output exists for op_id %q", p.OpID))
	}
	var offset int64
	if p.Cursor != nil {
		off, err := decodeCursor(p.OpID, *p.Cursor)
		if err != nil {
			return nil, gensurla.MakeUnprocessable(err)
		}
		offset = off
	}
	dir := filepath.Join(c.checkWorkDir, p.OpID)
	raw, err := os.ReadFile(filepath.Join(dir, outputMetaFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, gensurla.MakeNotFound(fmt.Errorf("no output exists for op_id %q; it may still be running or was removed", p.OpID))
	}
	if err != nil {
		return nil, err
	}
	meta, err := decodeOutputMeta(raw)
	if err != nil {
		return nil, fmt.Errorf("decode the output accounting of op_id %q: %w", p.OpID, err)
	}
	if offset > meta.Captured {
		return nil, gensurla.MakeUnprocessable(errors.New("cursor is past the end of the output"))
	}
	f, err := os.Open(filepath.Join(dir, outputFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, min(int64(maxCheckMessageBytes), meta.Captured-offset))
	n, err := f.ReadAt(buf, offset)
	if n < len(buf) {
		return nil, fmt.Errorf("the output of op_id %q ends before byte %d that its accounting records; its operation directory was changed: %w", p.OpID, offset+int64(len(buf)), err)
	}
	text, consumed := outputText(buf, offset+int64(len(buf)) == meta.Captured)
	page := &gensurla.CheckOutputPage{Text: text, CapturedBytes: meta.Captured, DroppedBytes: meta.Dropped}
	if next := offset + int64(consumed); next < meta.Captured {
		cursor := encodeCursor(p.OpID, next)
		page.NextCursor = &cursor
	}
	return page, nil
}

// outputText converts raw captured bytes into at most maxCheckMessageBytes
// of valid UTF-8 text, the text a caller receives, and returns it with the
// number of raw bytes it covers, where the next page starts. Each invalid
// byte becomes U+FFFD (3 bytes), as JSON encoding would make it, so the
// budget holds after transport. A character cut off at the end of raw is
// left for the next page unless atEnd. The first character always fits, so
// every page makes progress.
func outputText(raw []byte, atEnd bool) (string, int) {
	var b strings.Builder
	i := 0
	for i < len(raw) {
		if !atEnd && !utf8.FullRune(raw[i:]) && i > 0 {
			break
		}
		r, size := utf8.DecodeRune(raw[i:])
		piece := string(raw[i : i+size])
		if r == utf8.RuneError && size == 1 {
			piece = string(utf8.RuneError)
		}
		if b.Len()+len(piece) > maxCheckMessageBytes {
			break
		}
		b.WriteString(piece)
		i += size
	}
	return b.String(), i
}

// validText returns raw as valid UTF-8, each invalid byte replaced by
// U+FFFD as in outputText.
func validText(raw []byte) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.Write(raw[i : i+size])
		}
		i += size
	}
	return b.String()
}

// decodeOutputMeta strictly decodes a saved output accounting record: one
// JSON object with exactly the captured and dropped counts, both present
// and non-negative, and captured within the capture limit.
func decodeOutputMeta(raw []byte) (outputMeta, error) {
	var rec struct {
		Captured *int64 `json:"captured"`
		Dropped  *int64 `json:"dropped"`
	}
	if err := decodeStrictObject(raw, &rec); err != nil {
		return outputMeta{}, err
	}
	switch {
	case rec.Captured == nil || rec.Dropped == nil:
		return outputMeta{}, errors.New("captured and dropped are required")
	case *rec.Captured < 0 || *rec.Dropped < 0 || *rec.Captured > maxCheckLogBytes:
		return outputMeta{}, fmt.Errorf("captured %d and dropped %d are out of range", *rec.Captured, *rec.Dropped)
	}
	return outputMeta{Captured: *rec.Captured, Dropped: *rec.Dropped}, nil
}

// decodeStrictObject decodes raw, which must be exactly one JSON object
// (not null) with no unknown fields and no trailing data, into v.
func decodeStrictObject(raw []byte, v any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("the record is not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the record")
	}
	return nil
}

// encodeCursor returns the opaque cursor for offset within operation id.
func encodeCursor(id string, offset int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id + ":" + strconv.FormatInt(offset, 10)))
}

// decodeCursor validates cursor for operation id and returns its offset.
func decodeCursor(id, cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, errors.New("cursor is malformed")
	}
	owner, off, ok := strings.Cut(string(raw), ":")
	if !ok || owner != id {
		return 0, errors.New("cursor belongs to another operation")
	}
	offset, err := strconv.ParseInt(off, 10, 64)
	if err != nil || offset < 0 {
		return 0, errors.New("cursor is malformed")
	}
	return offset, nil
}
