// This file owns an operation's recovery record, owner.json, and the one
// decision that clears an entry from it: the record's version 2 form with
// each container's state, its strict decoding (including version 1
// records), its atomic writing, the listing-based removal decision shared
// by the check helper and recovery, and whether the operation's scratch can
// still be in use.
package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goadesign/surla/verify/candidate"
)

type (
	// checkOwner is an operation's recovery record, saved as owner.json
	// version 2: the engine every command targeted and every container whose
	// removal is not confirmed, each with what is known about it. The check
	// helper writes it; recovery rewrites it only once that helper has
	// exited.
	checkOwner struct {
		// Version is ownerVersion. A record without it is version 1, which
		// decodeCheckOwner reads with every name in stateUnknown.
		Version int `json:"version"`
		// Docker is the docker executable the operation used.
		Docker string `json:"docker"`
		// Host is the resolved local engine endpoint.
		Host string `json:"host"`
		// Containers are the containers whose removal is not confirmed, in
		// the order they were recorded, each name at most once.
		Containers []ownedContainer `json:"containers"`
	}

	// ownedContainer is one recorded container of an operation.
	ownedContainer struct {
		// Name is surla-check-<op>-<n>.
		Name string `json:"name"`
		// State is what is known about the container's creation and start.
		State containerState `json:"state"`
	}

	// containerState is one of exactly three facts about a recorded
	// container: stateCreationPending, stateCreationAnswered or
	// stateUnknown.
	containerState string
)

const (
	// ownerVersion is the owner.json version this server writes.
	ownerVersion = 2
	// stateCreationPending records a name before its create is sent: the
	// engine has not confirmed creating it, and the helper that recorded it
	// has not started it and never will once it returns. Such a container
	// may still appear later, but it never runs and mounts nothing.
	stateCreationPending containerState = "creation_pending"
	// stateCreationAnswered records that the engine confirmed creating the
	// container; it is saved before start is sent, so the container may have
	// started and may be using the operation's files.
	stateCreationAnswered containerState = "creation_answered"
	// stateUnknown is every name read from a version 1 record, which cannot
	// say whether its create was answered or the container started.
	stateUnknown containerState = "unknown"
)

var (
	// errCreateUnconfirmed explains a name kept because no listing has shown
	// its container: its create was never confirmed, or its record cannot
	// say whether it was, so it may still appear later.
	errCreateUnconfirmed = errors.New("its creation was never confirmed and no listing has shown it, so it may still appear")
	// errStillListed explains a name kept because a listing taken after its
	// removal still shows the container.
	errStillListed = errors.New("a listing after docker rm --force still shows it")
)

// decodeCheckOwner strictly decodes the recovery record of operation id:
// one JSON object with exactly the docker path (absolute), the local engine
// endpoint (unix://) and the containers array (present, possibly empty),
// each name surla-check-<id>-<n> and listed once. Version 2 has "version": 2
// and containers of {name, state} with state one of the three
// containerState values. Version 1 has no version and containers of names
// it recorded before sending each create, whether or not the create was
// answered or the container ran; its names are returned in stateUnknown.
// Recovery rewrites every record it reads as version 2, so the version 1
// branch can be deleted once no supported upgrade starts from a release
// that wrote version 1. Anything else is an error, so recovery never acts
// on a record it does not fully understand.
func decodeCheckOwner(raw []byte, id string) (checkOwner, error) {
	var rec struct {
		Version    int             `json:"version"`
		Docker     string          `json:"docker"`
		Host       string          `json:"host"`
		Containers json.RawMessage `json:"containers"`
	}
	if err := decodeStrictObject(raw, &rec); err != nil {
		return checkOwner{}, err
	}
	owner := checkOwner{Version: ownerVersion, Docker: rec.Docker, Host: rec.Host}
	switch rec.Version {
	case 0:
		var names []string
		if err := decodeStrictArray(rec.Containers, &names); err != nil {
			return checkOwner{}, err
		}
		owner.Containers = []ownedContainer{}
		for _, name := range names {
			owner.Containers = append(owner.Containers, ownedContainer{Name: name, State: stateUnknown})
		}
	case ownerVersion:
		if err := decodeStrictArray(rec.Containers, &owner.Containers); err != nil {
			return checkOwner{}, err
		}
	default:
		return checkOwner{}, fmt.Errorf("owner record version %d is not supported", rec.Version)
	}
	switch {
	case !filepath.IsAbs(owner.Docker):
		return checkOwner{}, fmt.Errorf("docker %q is not an absolute path", owner.Docker)
	case !strings.HasPrefix(owner.Host, "unix://"):
		return checkOwner{}, fmt.Errorf("host %q is not a local engine endpoint", owner.Host)
	}
	prefix := "surla-check-" + id + "-"
	seen := map[string]bool{}
	for _, c := range owner.Containers {
		n, ok := strings.CutPrefix(c.Name, prefix)
		switch {
		case !ok || n == "" || strings.Trim(n, "0123456789") != "":
			return checkOwner{}, fmt.Errorf("container %q does not belong to operation %s", c.Name, id)
		case seen[c.Name]:
			return checkOwner{}, fmt.Errorf("container %q is listed twice", c.Name)
		case c.State != stateCreationPending && c.State != stateCreationAnswered && c.State != stateUnknown:
			return checkOwner{}, fmt.Errorf("container %q has state %q, not %s, %s or %s", c.Name, c.State, stateCreationPending, stateCreationAnswered, stateUnknown)
		}
		seen[c.Name] = true
	}
	return owner, nil
}

// decodeStrictArray decodes raw, which must be one JSON array (not null or
// absent) with no unknown fields in its elements, into v.
func decodeStrictArray(raw json.RawMessage, v any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		return errors.New("containers is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeOwnerRecord atomically replaces dir/owner.json with owner as a
// version 2 record. Only the check helper of the operation, or recovery
// once that helper has exited, calls it.
func writeOwnerRecord(dir string, owner checkOwner) error {
	owner.Version = ownerVersion
	raw, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ownerFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", ownerFile, err)
	}
	return os.Rename(tmp, filepath.Join(dir, ownerFile))
}

// confirmRemoved is the one place the check helper and recovery decide
// whether a recorded container c of operation op is gone, so its entry may
// be cleared from owner.json. It never reads docker rm's output or treats
// its exit status as proof: a container is gone when its exact name is
// missing from a listing by the operation's label that completed
// (candidate.Engine.List). A failed listing proves nothing.
//
// A container in stateCreationAnswered is removed, then listed: it may be
// cleared once it is not listed. For stateCreationPending absence proves
// nothing, because a create that was never answered may still complete, and
// stateUnknown is treated the same way, because a version 1 name may come
// from a create still in flight: such an entry may be cleared only when a
// listing first shows the container, it is then removed, and a later
// listing no longer shows it; until a listing shows it, nothing is removed.
// This relies on the listing including containers in every state. It
// returns nil when the entry may be cleared and otherwise a
// *candidate.CleanupError saying why it is kept; an entry no listing has
// shown is kept with errCreateUnconfirmed.
func confirmRemoved(e *candidate.Engine, op string, c ownedContainer) error {
	keep := func(causes ...error) error {
		var errs []error
		for _, cause := range causes {
			var cleanup *candidate.CleanupError
			if errors.As(cause, &cleanup) {
				cause = cleanup.Err
			}
			errs = append(errs, cause)
		}
		return &candidate.CleanupError{Name: c.Name, Err: errors.Join(errs...)}
	}
	if c.State != stateCreationAnswered {
		listed, err := e.List(op)
		switch {
		case err != nil:
			return keep(err)
		case !slices.Contains(listed, c.Name):
			return keep(errCreateUnconfirmed)
		}
	}
	rmErr := e.Remove(c.Name)
	listed, err := e.List(op)
	switch {
	case err != nil:
		return keep(rmErr, err)
	case slices.Contains(listed, c.Name):
		return keep(rmErr, errStillListed)
	}
	return nil
}

// scratchUnused reports whether no recorded container can be using the
// operation's scratch: every entry left is stateCreationPending, which was
// never started. Only the operation's helper, before it returns, or
// recovery, once that helper has exited, asks, so no later start can make a
// pending container use it.
func (o checkOwner) scratchUnused() bool {
	for _, c := range o.Containers {
		if c.State != stateCreationPending {
			return false
		}
	}
	return true
}
