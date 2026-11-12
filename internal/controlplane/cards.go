// This file holds the control plane's Change Card store: one file per card
// under a directory that one process at a time may use.

package controlplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
)

type (
	// cardStore keeps every CP1 Change Card in its own file under one
	// directory, so cards outlive the process that wrote them. The files are
	// the only copy: each read decodes the file and each change replaces it.
	// One process at a time may use a directory; openCardStore holds an
	// exclusive lock on it until close. Callers hold ControlPlane.mu for
	// every call: the read lock for loads, the write lock for saves and close.
	cardStore struct {
		// dir holds one <hex sha256(change_id)>.json file per card.
		dir string
		// lock is the open lock file whose flock keeps other processes out.
		// close sets it to nil; the store then refuses to read or write, so
		// a request still running when the lock is released cannot write to
		// a directory another process may now hold.
		lock *os.File
	}

	// savedCard is the on-disk JSON representation of one Change Card. It
	// excludes the response-only UnverifiedApproval field.
	savedCard struct {
		// ChangeID is the Change Card identifier.
		ChangeID string
		// Status is the checkpoint status ("needs_confirmation" or "confirmed").
		Status string
		// Prompt is the operator feature request for this revision.
		Prompt string
		// Summary is the plain-English summary of requested behavior.
		Summary string
		// Ambiguities lists surfaced domain questions.
		Ambiguities []*gensurla.AmbiguityQuestion
		// Answers maps each ambiguity question ID to its confirmed answer.
		Answers map[string]string
		// Examples lists concrete examples mapped to evaluator checks.
		Examples []*gensurla.ExampleItem
		// PreservedBehaviors lists contract invariants preserved by this change.
		PreservedBehaviors []string
		// ConfirmedBy is the authenticated principal that confirmed the card.
		ConfirmedBy *string
		// RequestRevision is the SHA-256 digest of the canonical versioned card.
		RequestRevision string
		// UIComposition is the optional OpenUI Lang decision view for this revision.
		UIComposition *gensurla.UICompositionItem
		// Approval is the recorded Checkpoint 5 promotion approval for this revision.
		Approval *gensurla.ApprovalItem
	}

	// cardFile is the saved form of one card. Saved cards that do not match
	// this schema stop the calls that read them rather than being skipped.
	cardFile struct {
		// Card is the latest versioned Change Card.
		Card *savedCard `json:"card"`
		// Owner is the authenticated principal that created the card.
		Owner string `json:"owner"`
		// DesignSource is the Goa design source inspected at CP1.
		DesignSource string `json:"design_source"`
		// Stories holds the evaluated domain Stories adopted for this card
		// revision in declaration order.
		Stories []designeval.StorySummary `json:"stories"`
	}
)

const (
	// stagingFile is the name, inside a cards directory, of the file each save
	// writes before renaming it over the card's file. load opens only the hashed
	// path of the card it is asked for, so this file is never read.
	stagingFile = "pending"
)

var (
	// errCardsClosed reports a request that reached the card store after the
	// control plane was closed.
	errCardsClosed = errors.New("control plane is closed")
	// cardFileDigestPattern matches the 64-character lowercase hex digest of a
	// saved Change Card filename.
	cardFileDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// openCardStore resolves dir to its physical path, creates dir (0700) when
// missing, and takes its lock. It fails when another process already holds the
// lock.
func openCardStore(dir string) (*cardStore, error) {
	if dir == "" {
		return nil, errors.New("cards directory is required")
	}
	physDir, err := abspath.EvalMissing(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve cards directory: %w", err)
	}
	if err := os.MkdirAll(physDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cards directory: %w", err)
	}
	physDir, err = abspath.Eval(physDir)
	if err != nil {
		return nil, fmt.Errorf("resolve created cards directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(physDir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open cards directory lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = fmt.Errorf("cards directory %s is in use by another process", physDir)
		} else {
			err = fmt.Errorf("lock cards directory: %w", err)
		}
		return nil, errors.Join(err, lock.Close())
	}
	return &cardStore{dir: physDir, lock: lock}, nil
}

// syncDir flushes dir's entries so a completed rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// close releases the directory lock. Closing a closed store does nothing.
func (s *cardStore) close() error {
	if s.lock == nil {
		return nil
	}
	// A child between fork and exec can still hold a duplicate descriptor.
	// The store owns the lock lifetime, so release it before closing our copy.
	err := errors.Join(syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN), s.lock.Close())
	s.lock = nil
	return err
}

// load returns the saved card for changeID, or false when none was saved. A
// card file that cannot be read or decoded is an error.
func (s *cardStore) load(changeID string) (storedChangeCard, bool, error) {
	if s.lock == nil {
		return storedChangeCard{}, false, errCardsClosed
	}
	data, err := os.ReadFile(s.path(changeID))
	if errors.Is(err, fs.ErrNotExist) {
		return storedChangeCard{}, false, nil
	}
	if err != nil {
		return storedChangeCard{}, false, fmt.Errorf("read change card %q: %w", changeID, err)
	}
	rec, err := decodeCardBytes(changeID, filepath.Base(s.path(changeID)), true, data)
	if err != nil {
		return storedChangeCard{}, false, err
	}
	return rec, true, nil
}

// decodeCardBytes strictly decodes and validates a saved cardFile payload. When
// requireExactID is true, f.Card.ChangeID must equal label; otherwise label is
// the card filename and f.Card.ChangeID must hash to fileName.
func decodeCardBytes(label, fileName string, requireExactID bool, data []byte) (storedChangeCard, error) {
	var f cardFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return storedChangeCard{}, fmt.Errorf("decode change card %q: %w", label, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return storedChangeCard{}, fmt.Errorf("decode change card %q: trailing data", label)
	}
	if f.Card == nil || f.Card.ChangeID == "" || f.Owner == "" {
		return storedChangeCard{}, fmt.Errorf("change card file for %q does not hold that card", label)
	}
	if requireExactID && f.Card.ChangeID != label {
		return storedChangeCard{}, fmt.Errorf("change card file for %q does not hold that card", label)
	}
	sum := sha256.Sum256([]byte(f.Card.ChangeID))
	if hex.EncodeToString(sum[:])+".json" != fileName {
		return storedChangeCard{}, fmt.Errorf("change card file for %q does not hold that card", label)
	}
	stories := cloneStorySummaries(f.Stories)
	card := &gensurla.ChangeCardResult{
		ChangeID:           f.Card.ChangeID,
		Status:             f.Card.Status,
		Prompt:             f.Card.Prompt,
		Summary:            f.Card.Summary,
		Ambiguities:        f.Card.Ambiguities,
		Answers:            f.Card.Answers,
		Examples:           f.Card.Examples,
		PreservedBehaviors: f.Card.PreservedBehaviors,
		Stories:            storySummaryItemsOrNil(stories),
		ConfirmedBy:        f.Card.ConfirmedBy,
		RequestRevision:    f.Card.RequestRevision,
		UIComposition:      f.Card.UIComposition,
		Approval:           f.Card.Approval,
	}
	if card.Status != StatusNeedsConfirmation && card.Status != StatusConfirmed {
		return storedChangeCard{}, fmt.Errorf("change card %q has unknown status %q", label, card.Status)
	}
	// request_revision is the digest of the card's content, owner and design
	// source, so recomputing it checks every field it covers.
	rev, err := computeCanonicalRevision(card, f.Owner, f.DesignSource)
	if err != nil {
		return storedChangeCard{}, err
	}
	if rev != card.RequestRevision {
		return storedChangeCard{}, fmt.Errorf("change card %q does not match its request_revision", label)
	}
	return storedChangeCard{
		card:         card,
		owner:        f.Owner,
		designSource: f.DesignSource,
		stories:      stories,
	}, nil
}

// save replaces the saved card for record.card.ChangeID. It writes and syncs
// the card to the directory's one staging file, then renames that over the
// card's file. Only one save runs at a time in a directory (the directory
// lock and ControlPlane.mu), so one staging file suffices; a save that
// crashed leaves it behind, unread, and the next save overwrites it. An
// error before the rename leaves the previous card in place. The rename
// publishes the new card; an error syncing the directory after it means the
// new card is in place but may not survive a crash, and the error says so.
func (s *cardStore) save(record storedChangeCard) error {
	if s.lock == nil {
		return errCardsClosed
	}
	persisted := &savedCard{
		ChangeID:           record.card.ChangeID,
		Status:             record.card.Status,
		Prompt:             record.card.Prompt,
		Summary:            record.card.Summary,
		Ambiguities:        record.card.Ambiguities,
		Answers:            record.card.Answers,
		Examples:           record.card.Examples,
		PreservedBehaviors: record.card.PreservedBehaviors,
		ConfirmedBy:        record.card.ConfirmedBy,
		RequestRevision:    record.card.RequestRevision,
		UIComposition:      record.card.UIComposition,
		Approval:           record.card.Approval,
	}
	persistedStories := cloneStorySummaries(record.stories)
	if len(persistedStories) == 0 && len(record.card.Stories) > 0 {
		persistedStories = canonicalStorySummariesFromItems(record.card.Stories)
	}
	data, err := json.Marshal(cardFile{
		Card:         persisted,
		Owner:        record.owner,
		DesignSource: record.designSource,
		Stories:      persistedStories,
	})
	if err != nil {
		return fmt.Errorf("encode change card %q: %w", record.card.ChangeID, err)
	}
	staging := filepath.Join(s.dir, stagingFile)
	// O_NOFOLLOW: a link planted at the staging name must not redirect the
	// write to another file.
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	}
	if err == nil {
		err = os.Rename(staging, s.path(record.card.ChangeID))
	}
	if err != nil {
		return fmt.Errorf("save change card %q: %w", record.card.ChangeID, err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("change card %q is saved but may not survive a crash: %w", record.card.ChangeID, err)
	}
	return nil
}

// path names changeID's card file. Hashing the ID keeps any change_id inside
// the directory.
func (s *cardStore) path(changeID string) string {
	sum := sha256.Sum256([]byte(changeID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}

// hasWorkspaceCardsWithFiles reports whether any saved card in s captured
// existing workspace design files (non-empty design_source). Cards proposed
// in an empty workspace before init_project have
// an empty design_source and do not block initialization. Every saved card file
// is validated with the same strict decoder as load.
func (s *cardStore) hasWorkspaceCardsWithFiles() (bool, error) {
	if s.lock == nil {
		return false, errCardsClosed
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return false, fmt.Errorf("read cards directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isHexCardFilename(name) {
			continue
		}
		raw, err := readCardRegularFileNoFollow(filepath.Join(s.dir, name))
		if err != nil {
			return false, fmt.Errorf("read saved card %s: %w", name, err)
		}
		rec, err := decodeCardBytes(name, name, false, raw)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(rec.designSource) != "" {
			return true, nil
		}
	}
	return false, nil
}

// isHexCardFilename reports whether name is <64 lowercase hex>.json.
func isHexCardFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && cardFileDigestPattern.MatchString(strings.TrimSuffix(name, ".json"))
}

// readCardRegularFileNoFollow reads path without following symlinks and
// requires a regular file.
func readCardRegularFileNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	raw, readErr := io.ReadAll(f)
	return raw, errors.Join(readErr, f.Close())
}

// CardStoreInUse probes the existing writer lock without reading or changing any cards.
// The caller must serialize start decisions separately; the lock, not a PID record, proves liveness.
func CardStoreInUse(dir string) (inUse bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	return false, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
}
