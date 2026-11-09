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
	"slices"
	"strings"
	"syscall"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
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
	// holds only the 12 persisted card fields (omitting response-only
	// UnverifiedApproval) without omitempty tags so saved card files remain
	// byte-for-byte compatible across versions under DisallowUnknownFields.
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
		// SourceOrigin records whether DesignSource was read from the bound
		// workspace ("workspace") or supplied by the caller (omitted on disk).
		SourceOrigin string `json:"source_origin,omitempty"`
	}

	// canonicalAnswerPair is a deterministic key/value entry for JSON hashing.
	canonicalAnswerPair struct {
		// ID is the ambiguity question ID.
		ID string `json:"id"`
		// Answer is the selected option value.
		Answer string `json:"answer"`
	}

	// canonicalCardRevision is the canonical JSON structure hashed to produce
	// RequestRevision without delimiter ambiguity.
	canonicalCardRevision struct {
		// ChangeID is the Change Card identifier.
		ChangeID string `json:"change_id"`
		// Owner is the authenticated creator principal ID.
		Owner string `json:"owner"`
		// Status is the Change Card status.
		Status string `json:"status"`
		// Prompt is the original operator feature request.
		Prompt string `json:"prompt"`
		// DesignSource is the Goa design source inspected at CP1.
		DesignSource string `json:"design_source"`
		// Summary is the plain-English summary.
		Summary string `json:"summary"`
		// Ambiguities is the ordered slice of surfaced domain questions.
		Ambiguities []*gensurla.AmbiguityQuestion `json:"ambiguities"`
		// Answers is the key-sorted slice of confirmed ambiguity answers.
		Answers []canonicalAnswerPair `json:"answers"`
		// Examples is the ordered slice of confirmed example checks.
		Examples []*gensurla.ExampleItem `json:"examples"`
		// PreservedBehaviors is the ordered slice of preserved contract invariants.
		PreservedBehaviors []string `json:"preserved_behaviors"`
		// ConfirmedBy is the authenticated principal ID that confirmed the card.
		ConfirmedBy string `json:"confirmed_by"`
	}
)

const (
	// stagingFile is the name, inside a cards directory, of the file each save
	// writes before renaming it over the card's file. load opens only the hashed
	// path of the card it is asked for, so this file is never read.
	stagingFile = "pending"
	// sourceOriginSupplied marks a card whose design_source came from the caller
	// payload (or had no bound workspace at proposal time).
	sourceOriginSupplied = "supplied"
	// sourceOriginWorkspace marks a card whose design_source was read from the
	// control plane's bound workspace directory.
	sourceOriginWorkspace = "workspace"
)

var (
	// errCardsClosed reports a request that reached the card store after the
	// control plane was closed.
	errCardsClosed = errors.New("control plane is closed")
	// cardFileDigestPattern matches the 64-character lowercase hex digest of a
	// saved Change Card filename.
	cardFileDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// PrepareCardSnapshot copies saved <64-hex>.json Change Card files verbatim from
// legacyCardsDir into stageCardsDir while holding a non-blocking shared lock on
// <legacyCardsDir>/lock until releaseLock is called. It never imports
// workspace.json or modifies legacy originals.
func PrepareCardSnapshot(legacyCardsDir, stageCardsDir string) (releaseLock func() error, err error) {
	if err := os.MkdirAll(stageCardsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staged cards directory %s: %w", stageCardsDir, err)
	}
	if err := writeCardRegularFileNoFollow(filepath.Join(stageCardsDir, "lock"), nil, 0o600); err != nil {
		return nil, fmt.Errorf("create staged cards lock: %w", err)
	}
	if legacyCardsDir == "" {
		if err := syncDir(stageCardsDir); err != nil {
			return nil, fmt.Errorf("sync staged cards directory %s: %w", stageCardsDir, err)
		}
		return func() error { return nil }, nil
	}
	info, err := os.Lstat(legacyCardsDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := syncDir(stageCardsDir); err != nil {
			return nil, fmt.Errorf("sync staged cards directory %s: %w", stageCardsDir, err)
		}
		return func() error { return nil }, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat legacy cards directory %s: %w", legacyCardsDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("legacy cards directory %s is not a regular directory", legacyCardsDir)
	}

	var heldLock *os.File
	closeHeld := func() error {
		if heldLock == nil {
			return nil
		}
		f := heldLock
		heldLock = nil
		return f.Close()
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeHeld())
		}
	}()

	lockPath := filepath.Join(legacyCardsDir, "lock")
	if _, statErr := os.Lstat(lockPath); !errors.Is(statErr, fs.ErrNotExist) {
		f, openErr := os.OpenFile(lockPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			return nil, fmt.Errorf("open legacy cards lock %s: %w", lockPath, openErr)
		}
		st, statErr := f.Stat()
		if statErr == nil && !st.Mode().IsRegular() {
			statErr = fmt.Errorf("legacy cards lock %s is not a regular file", lockPath)
		}
		if statErr == nil {
			statErr = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		}
		if statErr != nil {
			if errors.Is(statErr, syscall.EWOULDBLOCK) {
				return nil, errors.Join(
					fmt.Errorf(
						"legacy cards directory %s has an active writer; stop all legacy surla processes before running -import-legacy-workspace",
						legacyCardsDir,
					),
					f.Close(),
				)
			}
			return nil, errors.Join(fmt.Errorf("lock legacy cards directory %s: %w", legacyCardsDir, statErr), f.Close())
		}
		heldLock = f
	}

	entries, err := os.ReadDir(legacyCardsDir)
	if err != nil {
		return nil, fmt.Errorf("read legacy cards directory %s: %w", legacyCardsDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !isHexCardFilename(name) {
			continue
		}
		srcPath := filepath.Join(legacyCardsDir, name)
		raw, err := readCardRegularFileNoFollow(srcPath)
		if err != nil {
			return nil, fmt.Errorf("read legacy card %s: %w", srcPath, err)
		}
		dstPath := filepath.Join(stageCardsDir, name)
		if err := writeCardRegularFileNoFollow(dstPath, raw, 0o600); err != nil {
			return nil, fmt.Errorf("write staged card %s: %w", dstPath, err)
		}
	}
	if err := syncDir(stageCardsDir); err != nil {
		return nil, fmt.Errorf("sync staged cards directory %s: %w", stageCardsDir, err)
	}
	return closeHeld, nil
}

// openCardStore resolves dir to its physical path, creates dir (0700) when
// missing, and takes its lock. It fails when another process already holds the
// lock.
func openCardStore(dir string) (*cardStore, error) {
	if strings.TrimSpace(dir) == "" {
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
	err := s.lock.Close()
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
	sourceOrigin := sourceOriginSupplied
	switch f.SourceOrigin {
	case "", sourceOriginSupplied:
		sourceOrigin = sourceOriginSupplied
	case sourceOriginWorkspace:
		sourceOrigin = sourceOriginWorkspace
	default:
		return storedChangeCard{}, fmt.Errorf("change card %q has unknown source_origin %q", label, f.SourceOrigin)
	}
	card := &gensurla.ChangeCardResult{
		ChangeID:           f.Card.ChangeID,
		Status:             f.Card.Status,
		Prompt:             f.Card.Prompt,
		Summary:            f.Card.Summary,
		Ambiguities:        f.Card.Ambiguities,
		Answers:            f.Card.Answers,
		Examples:           f.Card.Examples,
		PreservedBehaviors: f.Card.PreservedBehaviors,
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
		sourceOrigin: sourceOrigin,
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
	persistedOrigin := ""
	if record.sourceOrigin == sourceOriginWorkspace {
		persistedOrigin = sourceOriginWorkspace
	}
	data, err := json.Marshal(cardFile{
		Card:         persisted,
		Owner:        record.owner,
		DesignSource: record.designSource,
		SourceOrigin: persistedOrigin,
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
// existing workspace design files (source_origin == "workspace" with non-empty
// design_source). Cards proposed in an empty workspace before init_project have
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
		if rec.sourceOrigin == sourceOriginWorkspace && strings.TrimSpace(rec.designSource) != "" {
			return true, nil
		}
	}
	return false, nil
}

// isHexCardFilename reports whether name is <64 lowercase hex>.json.
func isHexCardFilename(name string) bool {
	return strings.HasSuffix(name, ".json") && cardFileDigestPattern.MatchString(strings.TrimSuffix(name, ".json"))
}

// computeCanonicalRevision computes a collision-free SHA-256 hex digest over
// the canonical JSON encoding of the ChangeCardResult, its owner principal, and
// its inspected design source.
func computeCanonicalRevision(card *gensurla.ChangeCardResult, owner, designSource string) (string, error) {
	keys := make([]string, 0, len(card.Answers))
	for k := range card.Answers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	answers := make([]canonicalAnswerPair, 0, len(keys))
	for _, k := range keys {
		answers = append(answers, canonicalAnswerPair{
			ID:     k,
			Answer: card.Answers[k],
		})
	}
	var confirmedBy string
	if card.ConfirmedBy != nil {
		confirmedBy = *card.ConfirmedBy
	}
	payload := canonicalCardRevision{
		ChangeID:           card.ChangeID,
		Owner:              owner,
		Status:             card.Status,
		Prompt:             card.Prompt,
		DesignSource:       designSource,
		Summary:            card.Summary,
		Ambiguities:        card.Ambiguities,
		Answers:            answers,
		Examples:           card.Examples,
		PreservedBehaviors: card.PreservedBehaviors,
		ConfirmedBy:        confirmedBy,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal canonical change card revision: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
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

// writeCardRegularFileNoFollow creates path exclusively without following
// symlinks, writes raw, syncs the file, sets perm, and closes it.
func writeCardRegularFileNoFollow(path string, raw []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return errors.Join(err, f.Close())
	}
	if perm != 0o600 {
		if err := f.Chmod(perm); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return errors.Join(f.Sync(), f.Close())
}
