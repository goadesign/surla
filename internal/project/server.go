// This file implements project management, persistent agent authentication, and memory-only browser sessions.
package project

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	genprojectserver "github.com/goadesign/surla/gen/http/project/server"
	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"go.opentelemetry.io/otel"
	goahttp "goa.design/goa/v3/http"
)

type (
	// Server is the sole owner of credentials for a running project. Browser credentials never leave memory.
	Server struct {
		state    *State
		stop     func()
		mu       sync.Mutex
		links    map[string]time.Time
		sessions map[string]string
		now      func() time.Time
	}
)

// NewServer binds the project owner and the composition root's ordered shutdown request.
func NewServer(state *State, stop func()) *Server {
	return &Server{state: state, stop: stop, links: map[string]time.Time{}, sessions: map[string]string{}, now: time.Now}
}

// Authenticate derives ownership and caller purpose solely from a trusted credential.
func (s *Server) Authenticate(r *http.Request) (controlplane.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return controlplane.Principal{}, errors.New("open the project's views with surla")
	}
	owner := s.state.Record.Owner
	s.mu.Lock()
	caller, ok := s.sessions[token]
	s.mu.Unlock()
	if ok {
		if r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			return controlplane.Principal{}, errors.New("browser session requires a same-origin page request")
		}
		return controlplane.Principal{ID: owner, CallerID: caller, Kind: controlplane.CallerUser}, nil
	}
	management, err := s.state.ManagementCredential()
	if err != nil {
		return controlplane.Principal{}, err
	}
	if credentialEqual(token, management) {
		return controlplane.Principal{ID: owner, CallerID: "management", Kind: controlplane.CallerManagement}, nil
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	for id, role := range s.state.Record.Agents {
		raw, err := readPrivate(s.state.AgentFile(id))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return controlplane.Principal{}, err
		}
		if credentialEqual(token, string(raw)) {
			return controlplane.Principal{ID: owner, CallerID: id, Kind: controlplane.CallerKind(role)}, nil
		}
	}
	return controlplane.Principal{}, errors.New("session or credential is no longer valid; open the project's views with surla")
}

// Handler mounts the generated project contract and guards every page, HTTP, and MCP response.
// The composition root owns the single public GET /views shell in application.
func (s *Server) Handler(application http.Handler) http.Handler {
	mux := goahttp.NewMuxer()
	endpoints := genproject.NewEndpoints(s)
	server := genprojectserver.New(endpoints, mux, strictDecoder, goahttp.ResponseEncoder, nil, nil)
	genprojectserver.Mount(mux, server)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != s.state.Address() || r.Header.Get("Origin") != "" && r.Header.Get("Origin") != s.state.Origin() {
			writeAccessError(w, r, http.StatusForbidden, "forbidden", "request must use the project's own address and page")
			return
		}
		site := r.Header.Get("Sec-Fetch-Site")
		shell := r.Method == http.MethodGet && r.URL.Path == "/views"
		if site != "" && site != "same-origin" && !(shell && (site == "none" || site == "same-site")) {
			writeAccessError(w, r, http.StatusForbidden, "forbidden", "request must come from the project's own page")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/project/") {
			if r.URL.Path == "/project/browser-session" && r.Method == http.MethodPost {
				if site != "same-origin" {
					writeAccessError(w, r, http.StatusForbidden, "forbidden", "open the project's views with surla")
					return
				}
			} else {
				principal, err := s.Authenticate(r)
				if err != nil {
					writeAccessError(w, r, http.StatusUnauthorized, "unauthorized", "open the project's views with surla")
					return
				}
				r = r.WithContext(controlplane.WithPrincipal(r.Context(), principal))
			}
			mux.ServeHTTP(w, r)
			return
		}
		application.ServeHTTP(w, r)
	})
}

// Identity proves the answering address belongs to the CLI's project.
func (s *Server) Identity(ctx context.Context) (*genproject.ProjectIdentity, error) {
	if err := managementOnly(ctx); err != nil {
		return nil, err
	}
	r := s.state.Record
	return &genproject.ProjectIdentity{ProjectID: r.ID, Directory: r.Directory, Owner: r.Owner}, nil
}

// Stop requests the composition root's ordered shutdown without blocking its own HTTP request.
func (s *Server) Stop(ctx context.Context) error {
	if err := managementOnly(ctx); err != nil {
		return err
	}
	s.stop()
	return nil
}

// AddAgent publishes a credential file and its immutable role as one admitted management operation.
func (s *Server) AddAgent(ctx context.Context, p *genproject.AddAgentPayload) (*genproject.ProjectAgent, error) {
	if err := managementOnly(ctx); err != nil {
		return nil, err
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if _, exists := s.state.Record.Agents[p.ActorID]; exists {
		return nil, genproject.MakeConflict(errors.New("agent already exists"))
	}
	token, err := randomCredential()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.state.Dir, "agents"), 0o700); err != nil {
		return nil, err
	}
	if err := privateDirectory(filepath.Join(s.state.Dir, "agents")); err != nil {
		return nil, err
	}
	if err := writePrivate(s.state.AgentFile(p.ActorID), []byte(token)); err != nil {
		return nil, err
	}
	s.state.Record.Agents[p.ActorID] = p.Role
	if err := s.state.save(); err != nil {
		delete(s.state.Record.Agents, p.ActorID)
		return nil, errors.Join(err, os.Remove(s.state.AgentFile(p.ActorID)))
	}
	return &genproject.ProjectAgent{ActorID: p.ActorID, Role: p.Role, CredentialFile: s.state.AgentFile(p.ActorID)}, nil
}

// RemoveAgent revokes the credential before persisting removal of its roster identity.
func (s *Server) RemoveAgent(ctx context.Context, p *genproject.RemoveAgentPayload) error {
	if err := managementOnly(ctx); err != nil {
		return err
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if _, exists := s.state.Record.Agents[p.ActorID]; !exists {
		return genproject.MakeUnprocessable(errors.New("unknown agent"))
	}
	if err := os.Remove(s.state.AgentFile(p.ActorID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(s.state.Record.Agents, p.ActorID)
	return s.state.save()
}

// BrowserLink creates a short-lived value usable for one browser exchange only.
func (s *Server) BrowserLink(ctx context.Context) (*genproject.BrowserLinkResult, error) {
	if err := managementOnly(ctx); err != nil {
		return nil, err
	}
	token, err := randomCredential()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for link, expires := range s.links {
		if !now.Before(expires) {
			delete(s.links, link)
		}
	}
	expires := now.Add(2 * time.Minute)
	s.links[token] = expires
	return &genproject.BrowserLinkResult{URL: s.state.Origin() + "/views#link=" + token, ExpiresAt: expires.Format(time.RFC3339Nano)}, nil
}

// ExchangeBrowserLink atomically consumes one valid link and creates a random memory-only session.
func (s *Server) ExchangeBrowserLink(_ context.Context, p *genproject.ExchangeBrowserLinkPayload) (*genproject.ExchangeBrowserLinkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, exists := s.links[p.Link]
	if !exists || !s.now().Before(expires) {
		delete(s.links, p.Link)
		return nil, genproject.MakeUnprocessable(errors.New("browser link is used or expired; open the project's views with surla"))
	}
	token, err := randomCredential()
	if err != nil {
		return nil, err
	}
	id, err := randomCredential()
	if err != nil {
		return nil, err
	}
	delete(s.links, p.Link)
	s.sessions[token] = "user-" + id
	return &genproject.ExchangeBrowserLinkResult{Session: token}, nil
}

// managementOnly checks trusted caller purpose rather than naming conventions or payload claims.
func managementOnly(ctx context.Context) error {
	p, ok := controlplane.PrincipalFromContext(ctx)
	if !ok || p.Kind != controlplane.CallerManagement {
		return genproject.MakeForbidden(errors.New("this operation requires the surla command's management credential"))
	}
	return nil
}

// credentialEqual compares opaque credentials without leaking their common prefix.
func credentialEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// strictDecoder rejects unknown fields and trailing JSON using the shared HTTP boundary.
func strictDecoder(r *http.Request) goahttp.Decoder { return controlplane.StrictJSONRequestDecoder(r) }

// writeAccessError preserves the generated service error shape even before routing or authentication.
func writeAccessError(w http.ResponseWriter, r *http.Request, status int, name, message string) {
	accessErr := genproject.MakeForbidden(fmt.Errorf("%s", message))
	accessErr.Name = name
	response := goahttp.NewErrorResponse(r.Context(), accessErr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := goahttp.ResponseEncoder(r.Context(), w).Encode(response); err != nil {
		otel.Handle(err)
	}
}
