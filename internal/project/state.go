// Package project owns the private state and credentials of one local Surla project.
package project

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/abspath"
)

type (
	// State is one physical project's durable identity. The server holds its card-store lock while changing it.
	State struct {
		// Dir holds every private artifact belonging to this project.
		Dir string
		// Record is read or changed under mu after the start lock or card-store lock is acquired.
		Record Record
		// mu serializes the running server's credential and record changes.
		mu sync.Mutex
	}
	// Record is the durable project identity; agent credentials live in separate private files.
	Record struct {
		// ID is the stable project identity checked by the CLI at the saved address.
		ID string `json:"id"`
		// Directory is the physical folder, independent of any Git root.
		Directory string `json:"directory"`
		// Owner is the immutable operating-system account identity.
		Owner string `json:"owner"`
		// Port is selected once and reused so existing agent connections survive restart.
		Port int `json:"port"`
		// Team is the stable Chorus team name, selected once by the server.
		Team string `json:"team,omitempty"`
		// Space is the most recently adopted local Herdr space.
		Space string `json:"space,omitempty"`
		// Agents maps each stable actor ID to its trusted role and working folder; secrets are separate files.
		Agents map[string]agentRecord `json:"agents"`
	}
	// agentRecord is the sole persisted role and working-folder contract for one stable handle.
	agentRecord struct {
		Role   string `json:"role"`
		Folder string `json:"folder"`
	}
	// Lock serializes CLI starts until an answering server owns the existing store lock.
	Lock struct {
		// file holds the exclusive project start lock until Close.
		file *os.File
	}
	// Running publishes one server's management access, never the authority for liveness.
	Running struct {
		// PID identifies the owned process for diagnostics, never for liveness decisions.
		PID int `json:"pid"`
		// Terminal identifies the Herdr terminal that owns this server program.
		Terminal string `json:"terminal"`
		// ManagementCredential is accepted only by the server that published this record.
		ManagementCredential string `json:"management_credential"`
	}
)

// freePortAttempts bounds how many free ports Initialize asks the kernel for while each
// offer is a port another project saved. The kernel offers ports across its ephemeral
// range, so even many saved ports rarely take a second try; the bound only ends the loop.
const freePortAttempts = 20

// Discover finds the nearest existing physical project record, or prepares a new project folder.
// It never considers the Git root. Call Initialize only after StartLock.
func Discover(root, directory string) (*State, error) {
	physical, err := abspath.Stored(directory)
	if err != nil {
		return nil, err
	}
	for candidate := physical; ; candidate = filepath.Dir(candidate) {
		dir := projectDirectory(root, candidate)
		var record Record
		err := readJSON(filepath.Join(dir, "project.json"), &record)
		if err == nil {
			if err := validateRecord(record, candidate); err != nil {
				return nil, err
			}
			return &State{Dir: dir, Record: record}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if filepath.Dir(candidate) == candidate {
			break
		}
	}
	return &State{Dir: projectDirectory(root, physical), Record: Record{Directory: physical}}, nil
}

// Open loads the initialized project whose private state folder dir the
// `surla` command found with Discover and passed to the project server it
// launched, so the server serves exactly that state and never derives a
// folder from its own environment. dir must be the absolute, physical
// (symlink-free) path of a folder private to this account, hold a valid
// project record whose project folder exists and is named by its stored
// physical path, carry the name Discover derives from that folder, and lie
// outside it; otherwise Open refuses, naming dir and
// never a credential. A folder without a record was never initialized by `surla`.
func Open(dir string) (*State, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("project state folder %q is not an absolute path; start the project with surla", dir)
	}
	if err := privateDirectory(dir); err != nil {
		return nil, fmt.Errorf("project state folder %s: %w", dir, err)
	}
	physical, err := abspath.Eval(dir)
	if err != nil {
		return nil, fmt.Errorf("project state folder %s: %w", dir, err)
	}
	if physical != dir {
		return nil, fmt.Errorf("project state folder %s is not its physical path %s; start the project with surla", dir, physical)
	}
	var record Record
	err = readJSON(filepath.Join(dir, "project.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("project state folder %s holds no project; start the project with surla", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("project state folder %s: %w", dir, err)
	}
	// The record's project folder is compared with the path the volume stores
	// for it, resolved here as Discover resolves it and never taken from the
	// record itself: a record that names its folder by a relative path, through
	// a link or in another spelling is not one `surla` wrote.
	if !filepath.IsAbs(record.Directory) {
		return nil, fmt.Errorf("project state folder %s: invalid project record", dir)
	}
	project, err := abspath.Stored(record.Directory)
	if err != nil {
		return nil, fmt.Errorf("project state folder %s: project folder %s: %w", dir, record.Directory, err)
	}
	if project != record.Directory {
		return nil, fmt.Errorf("project state folder %s names its project folder %s, which is not that folder's stored path %s", dir, record.Directory, project)
	}
	if err := validateRecord(record, project); err != nil {
		return nil, fmt.Errorf("project state folder %s: %w", dir, err)
	}
	if projectDirectory(filepath.Dir(dir), record.Directory) != dir {
		return nil, fmt.Errorf("project state folder %s does not belong to the project folder %s", dir, record.Directory)
	}
	inside, err := abspath.Within(record.Directory, dir)
	if err != nil {
		return nil, fmt.Errorf("project state folder %s: %w", dir, err)
	}
	if inside {
		return nil, fmt.Errorf("project state folder %s must lie outside the project folder %s", dir, record.Directory)
	}
	return &State{Dir: dir, Record: record}, nil
}

// StartLock bounds waiting for a concurrent start and keeps start/attach/stop decisions serialized.
func (s *State) StartLock(ctx context.Context) (*Lock, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := privateDirectory(s.Dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "start.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(err, f.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), f.Close())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Close releases a start lock after the server has answered or the attempt has failed.
func (l *Lock) Close() error {
	return errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
}

// Initialize creates the owner and fixed port once under StartLock.
func (s *State) Initialize(owner string) (bool, error) {
	return s.initialize(owner, freeLoopbackPort)
}

// initialize is Initialize with the source of free ports, which tests replace.
func (s *State) initialize(owner string, freePort func() (int, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var record Record
	err := readJSON(filepath.Join(s.Dir, "project.json"), &record)
	if err == nil {
		if err := validateRecord(record, s.Record.Directory); err != nil {
			return false, err
		}
		s.Record = record
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	id, err := randomCredential()
	if err != nil {
		return false, err
	}
	others, err := s.otherProjects()
	if err != nil {
		return false, err
	}
	saved := make(map[int]bool, len(others))
	for _, other := range others {
		saved[other.Record.Port] = true
	}
	port := 0
	for attempt := 0; port == 0; attempt++ {
		if attempt == freePortAttempts {
			return false, fmt.Errorf("no free loopback port that another Surla project has not saved was found in %d tries; run surla again", freePortAttempts)
		}
		offered, err := freePort()
		if err != nil {
			return false, err
		}
		if !saved[offered] {
			port = offered
		}
	}
	s.Record = Record{ID: id, Directory: s.Record.Directory, Owner: owner, Port: port, Agents: map[string]agentRecord{}}
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
}

// ProjectsSharingPort returns the other projects of this account, under the same operator
// folder, that saved this project's port. Only projects created before new projects
// avoided saved ports can share one; the CLI names such a project when its server holds
// the port. A record that cannot be read is skipped, as otherProjects explains.
func (s *State) ProjectsSharingPort() ([]*State, error) {
	others, err := s.otherProjects()
	if err != nil {
		return nil, err
	}
	sharing := make([]*State, 0, len(others))
	for _, other := range others {
		if other.Record.Port == s.Record.Port {
			sharing = append(sharing, other)
		}
	}
	return sharing, nil
}

// Address is the sole permitted HTTP authority, reused after every server restart.
func (s *State) Address() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Record.Port)) }

// Origin is the exact browser origin; localhost and other loopback spellings are refused.
func (s *State) Origin() string { return "http://" + s.Address() }

// CardsDir is the existing writer-lock owner's directory inside this project's state.
func (s *State) CardsDir() string { return filepath.Join(s.Dir, "cards") }

// AgentFile names a private credential whose contents never belong in an argument or label.
func (s *State) AgentFile(id string) string { return filepath.Join(s.Dir, "agents", id) }

// SaveRunning records a server that already holds the existing writer lock and listener.
func (s *State) SaveRunning(record Running) error {
	return writeJSON(filepath.Join(s.Dir, "running.json"), record)
}

// RemoveRunning withdraws management access before the listener closes, or removes stale startup state.
func (s *State) RemoveRunning() error {
	err := os.Remove(filepath.Join(s.Dir, "running.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ReadRunning reads the last server record; only the writer lock establishes liveness.
func (s *State) ReadRunning() (Running, error) {
	var r Running
	if err := readJSON(filepath.Join(s.Dir, "running.json"), &r); err != nil {
		return Running{}, err
	}
	if len(r.ManagementCredential) != 64 {
		return Running{}, errors.New("invalid running management credential")
	}
	if _, err := hex.DecodeString(r.ManagementCredential); err != nil {
		return Running{}, errors.New("invalid running management credential")
	}
	return r, nil
}

// ReadSettings returns one owner-private startup configuration. The CLI validates it before SaveSettings.
func (s *State) ReadSettings(kind string) ([]byte, error) {
	if kind != "verification" && kind != "promotion" {
		return nil, errors.New("unknown project settings")
	}
	return readPrivate(filepath.Join(s.Dir, kind+".json"))
}

// SaveSettings atomically replaces a validated startup configuration under the caller's StartLock.
func (s *State) SaveSettings(kind string, raw []byte) error {
	if kind != "verification" && kind != "promotion" {
		return errors.New("unknown project settings")
	}
	return writePrivate(filepath.Join(s.Dir, kind+".json"), raw)
}

// RemoveSettings removes only the named startup settings, taking effect at the next server start.
func (s *State) RemoveSettings(kind string) error {
	if kind != "verification" && kind != "promotion" {
		return errors.New("unknown project settings")
	}
	err := os.Remove(filepath.Join(s.Dir, kind+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// otherProjects reads the saved records of the other projects in this project's operator
// folder, where Discover places every project of this account. A record that cannot be
// read is skipped rather than failing this project: its own project cannot start until
// it is repaired, and Discover reports it, with its path, in that project's folder.
func (s *State) otherProjects() ([]*State, error) {
	root := filepath.Dir(s.Dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	others := make([]*State, 0, len(entries))
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "project-") || dir == s.Dir {
			continue
		}
		var record Record
		if err := readJSON(filepath.Join(dir, "project.json"), &record); err != nil {
			continue
		}
		others = append(others, &State{Dir: dir, Record: record})
	}
	return others, nil
}

// freeLoopbackPort briefly binds and releases a loopback port the kernel reports free.
// The answer is a snapshot: another process can take the port before the server binds it.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// projectDirectory derives one private state folder from the canonical folder identity.
func projectDirectory(root, directory string) string {
	sum := sha256.Sum256([]byte(directory))
	return filepath.Join(root, "project-"+hex.EncodeToString(sum[:]))
}

// validateRecord rejects missing or changed durable identity instead of silently creating another store.
func validateRecord(r Record, directory string) error {
	if len(r.ID) != 64 || r.Owner == "" || r.Directory != directory || r.Port < 1 || r.Port > 65535 || r.Agents == nil {
		return errors.New("invalid project record")
	}
	if _, err := hex.DecodeString(r.ID); err != nil {
		return errors.New("invalid project identity")
	}
	if r.Team != "" && !validTeamName(r.Team) {
		return errors.New("invalid project team name")
	}
	if r.Space != "" && !safeSpaceID(r.Space) {
		return errors.New("invalid project space")
	}
	seenRoles := map[string]bool{}
	for actor, agent := range r.Agents {
		if err := validateAgent(actor, agent); err != nil {
			return fmt.Errorf("invalid stored agent: %w", err)
		}
		if seenRoles[agent.Role] {
			return fmt.Errorf("invalid stored role %q: more than one agent is registered", agent.Role)
		}
		seenRoles[agent.Role] = true
	}
	return nil
}

// save persists the single project record while its owning lock is held.
func (s *State) save() error { return writeJSON(filepath.Join(s.Dir, "project.json"), s.Record) }

// randomCredential provides opaque credentials; no durable key can recreate browser sessions.
func randomCredential() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// readJSON rejects unknown fields, trailing data, and unsafe private files.
func readJSON(path string, value any) error {
	raw, err := readPrivate(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("decode project state: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing project state")
	}
	return nil
}

// writeJSON writes the complete owner-private current-format document.
func writeJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePrivate(path, raw)
}

// privateDirectory refuses symlinks, permissive directories, and another account's state.
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("project state directory must be private and owned by this account")
	}
	return nil
}

// readPrivate refuses links and non-private files before reading their credential-bearing bytes.
func readPrivate(path string) (raw []byte, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || int(st.Uid) != os.Geteuid() {
		return nil, errors.New("project state file must be private and owned by this account")
	}
	return io.ReadAll(f)
}

// writePrivate atomically publishes and syncs a private file, never following a destination symlink.
func writePrivate(path string, raw []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		removeErr := os.Remove(name)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := f.Write(raw)
	if err = errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// readCredential validates the persisted random-token format without exposing credential bytes in errors.
func readCredential(path string) (string, error) {
	raw, err := readPrivate(path)
	if err != nil {
		return "", err
	}
	if len(raw) != 64 {
		return "", errors.New("invalid credential file")
	}
	if _, err := hex.DecodeString(string(raw)); err != nil {
		return "", errors.New("invalid credential file")
	}
	return string(raw), nil
}

// validateAgent checks the private stored contract without relying on removed HTTP methods.
func validateAgent(actor string, agent agentRecord) error {
	if !validTeamName(actor) {
		return errors.New("agent handle must be a Chorus identifier of at most 32 characters")
	}
	if agent.Role != "guide" && agent.Role != "builder" && agent.Role != "test_writer" {
		return errors.New("agent role must be guide, builder or test_writer")
	}
	if !filepath.IsAbs(agent.Folder) || filepath.Clean(agent.Folder) != agent.Folder || strings.ContainsRune(agent.Folder, 0) {
		return errors.New("agent folder must be an absolute clean directory path")
	}
	return nil
}
