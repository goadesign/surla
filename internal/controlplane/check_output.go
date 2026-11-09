// This file implements read_check_output: bounded pages of one check
// operation's captured output, with an opaque code-owned cursor.
package controlplane

import (
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

	gensurla "github.com/goadesign/surla/gen/surla"
)

// opIDPattern is the shape of an operation ID: 32 lowercase hex digits.
var opIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ReadCheckOutput returns one page of at most 16 KiB of an operation's
// captured output, starting at the cursor, with the next cursor and the
// operation's captured and dropped byte counts.
func (c *ControlPlane) ReadCheckOutput(_ context.Context, p *gensurla.ReadCheckOutputPayload) (*gensurla.CheckOutputPage, error) {
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
	var meta outputMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("decode output accounting: %w", err)
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
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	page := &gensurla.CheckOutputPage{Text: string(buf[:n]), CapturedBytes: meta.Captured, DroppedBytes: meta.Dropped}
	if next := offset + int64(n); next < meta.Captured {
		cursor := encodeCursor(p.OpID, next)
		page.NextCursor = &cursor
	}
	return page, nil
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
