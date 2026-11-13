// This file composes the per-project server with its Herdr-owned lifetime.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"go.opentelemetry.io/otel"
	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// projectRuntime binds project state and process effects at the CLI composition root.
// Tests provide an owned state root and stand-ins without starting terminals or browsers.
type projectRuntime struct {
	// configRoot is supplied by the process composition root, never a package global.
	configRoot     func() (string, error)
	preflight      func() error
	startTerminal  func(context.Context, *project.State) (string, error)
	openViews      func(context.Context, string) error
	waitTerminal   func(context.Context, string) (bool, error)
	removeTerminal func(context.Context, string) error
	// listen binds only the saved project address; tests observe the actual close boundary.
	listen func(string, string) (net.Listener, error)
	// teamDependencies binds the server login environment and accepted-work lifetime.
	teamDependencies func(context.Context) (project.TeamDependencies, error)
	// startTeamPrograms starts absent desktop/service dependencies after server readiness.
	startTeamPrograms func(context.Context) error
}

// ownedProjectListener ends management authority before every underlying listener close,
// including the close deferred inside http.Server.Serve on an accept failure.
type ownedProjectListener struct {
	net.Listener
	management *project.Server
	// once prevents an old owner's repeated Close from removing its successor's record.
	once sync.Once
	// err preserves the first revocation and underlying-close result for every caller.
	err error
}

// productionProjectRuntime is selected only by the executable's composition root.
func productionProjectRuntime() projectRuntime {
	return projectRuntime{
		configRoot: os.UserConfigDir,
		preflight: func() error {
			commands, err := resolveTeamCommands(exec.LookPath, runTeamCommand)
			if err != nil {
				return err
			}
			if err := preflightTeam(commands); err != nil {
				return err
			}
			return preflightProjectStart(projectHerdrProgram)
		},
		teamDependencies: projectTeamDependencies,
		startTeamPrograms: func(ctx context.Context) error {
			commands, err := resolveTeamCommands(exec.LookPath, runTeamCommand)
			if err != nil {
				return err
			}
			commands.start = startDetachedTeamProgram
			return startTeamPrograms(ctx, commands)
		},
		startTerminal: func(ctx context.Context, state *project.State) (string, error) {
			return startProjectTerminal(ctx, state, projectHerdrProgram)
		},
		openViews:      openProjectViews,
		waitTerminal:   waitProjectTerminal,
		removeTerminal: removeProjectTerminal,
		listen:         net.Listen,
	}
}

// start serializes discovery and starts at most one server, then opens a one-use views link.
func (runtime projectRuntime) start(stdout io.Writer) (err error) {
	if err := runtime.preflight(); err != nil {
		return err
	}
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// Observe only the terminal this CLI creates; an existing server has no launch to watch.
	type terminalResult struct {
		ended bool
		err   error
	}
	terminal := ""
	finished := make(chan terminalResult, 1)
	client, created, err := attachOrStartProject(ctx, state, func(ctx context.Context, state *project.State) (string, error) {
		id, err := runtime.startTerminal(ctx, state)
		if err != nil {
			return "", err
		}
		terminal = id
		go func() {
			ended, err := runtime.waitTerminal(ctx, id)
			finished <- terminalResult{ended: ended, err: err}
			if ended || err != nil {
				cancel()
			}
		}()
		return id, nil
	})
	if terminal != "" {
		// Reap the observer on readiness, failure, or deadline before returning.
		cancel()
		outcome := <-finished
		if err != nil {
			err = errors.Join(err, outcome.err)
			if outcome.ended {
				cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				err = errors.Join(err, runtime.removeTerminal(cleanupCtx, terminal))
				stop()
			}
		}
	}
	if created {
		err = errors.Join(err, writeProjectCreatedNotice(stdout))
	}
	if err != nil {
		return err
	}

	if err := runtime.startTeamPrograms(context.Background()); err != nil {
		return err
	}
	teamCtx, stopTeam := context.WithTimeout(context.Background(), 50*time.Second)
	defer stopTeam()
	team, err := client.EnsureTeam(teamCtx)
	if err != nil {
		return err
	}
	if team.Dialog == "already_open" {
		_, err = fmt.Fprintf(stdout, "Team %s.\nThe Chorus launcher is already open in Herdr; choose your agents there if it is for this project, otherwise close it and run surla again\n", team.Team)
	} else if team.Dialog == "not_needed" {
		_, err = fmt.Fprintf(stdout, "Team %s is running.\n", team.Team)
	} else {
		_, err = fmt.Fprintf(stdout, "Team %s: choose an agent in the open Chorus dialog in Herdr.\n", team.Team)
	}
	if err != nil {
		return err
	}

	linkCtx, stopLink := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopLink()
	link, err := client.BrowserLink(linkCtx)
	if err != nil {
		return err
	}
	if err := validateProjectViewsLink(state, link.URL); err != nil {
		return err
	}
	if err := runtime.openViews(linkCtx, link.URL); err != nil {
		return fmt.Errorf("open project views: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "Surla is running for %s at %s\n", state.Record.Directory, state.Origin())
	return err
}

// validateProjectViewsLink confines the browser opener to the project's exact views
// address and opaque link format, without returning an untrusted URL in an error.
func validateProjectViewsLink(state *project.State, address string) error {
	token, ok := strings.CutPrefix(address, state.Origin()+"/views#link=")
	if !ok || len(token) != 64 {
		return errors.New("project server returned an invalid views link; run surla again")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return errors.New("project server returned an invalid views link; run surla again")
	}
	return nil
}

// writeProjectCreatedNotice explains the folder-identity rule whenever the CLI creates a project.
func writeProjectCreatedNotice(stdout io.Writer) error {
	_, err := fmt.Fprintln(stdout, "Created a new, empty Surla project. Moving or renaming this folder starts a new project.")
	return err
}

// attachOrStartProject holds the start lock until one authenticated server answers.
func attachOrStartProject(
	ctx context.Context,
	state *project.State,
	launch func(context.Context, *project.State) (string, error),
) (client *genproject.Client, created bool, err error) {
	lock, err := state.StartLock(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	created, err = state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
	if err != nil {
		return nil, created, err
	}
	inUse, err := controlplane.CardStoreInUse(state.CardsDir())
	if err != nil {
		return nil, created, err
	}
	terminal := ""
	if !inUse {
		if err := state.RemoveRunning(); err != nil {
			return nil, created, err
		}
		listener, err := net.Listen("tcp", state.Address())
		if err != nil {
			return nil, created, fmt.Errorf("project port %d is occupied; no server was started: %w", state.Record.Port, err)
		}
		if err := listener.Close(); err != nil {
			return nil, created, err
		}
		if terminal, err = launch(ctx, state); err != nil {
			return nil, created, err
		}
	}
	client = projectClient(state)
	if err := confirmProject(ctx, state, client); err != nil {
		if inUse {
			return nil, created, fmt.Errorf("%s; no second server was started: %w", projectHolder(state), err)
		}
		return nil, created, fmt.Errorf("the project server in Herdr terminal %s did not start answering: %w", terminal, err)
	}
	return client, created, nil
}

// stop authenticates the current project and waits until ordered cleanup releases the store.
func (runtime projectRuntime) stop(args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("stop takes no arguments")
	}
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	if state.Record.ID == "" {
		return errors.New("this folder has no Surla project")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lock, err := state.StartLock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	inUse, err := controlplane.CardStoreInUse(state.CardsDir())
	if err != nil {
		return err
	}
	if !inUse {
		_, err := fmt.Fprintln(stdout, "Surla is stopped.")
		return err
	}
	client := projectClient(state)
	if err := confirmProject(ctx, state, client); err != nil {
		return fmt.Errorf("%s: %w", projectHolder(state), err)
	}
	if err := client.Stop(ctx); err != nil {
		return err
	}
	for {
		held, err := controlplane.CardStoreInUse(state.CardsDir())
		if err != nil {
			return err
		}
		if !held {
			_, err := fmt.Fprintln(stdout, "Surla stopped.")
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server has not finished stopping: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// projectHolder describes a held lock without claiming the listener is ready.
// Terminal metadata is optional; unavailable metadata does not change the lock's fact.
func projectHolder(state *project.State) string {
	if running, err := state.ReadRunning(); err == nil && running.Terminal != "" {
		return fmt.Sprintf("a server in Herdr terminal %s holds this project and may still be starting or stopping", running.Terminal)
	}
	return "a server holds this project and may still be starting or stopping"
}

// serve is the Herdr program entry, with project identity and address derived from private state.
func (runtime projectRuntime) serve() (err error) {
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	if state.Record.ID == "" {
		return errors.New("start the project with surla")
	}
	provider, err := newTraceProvider(context.Background(), os.Stderr, os.Getenv)
	if err != nil {
		return err
	}
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = errors.Join(err, provider.Shutdown(ctx))
		otel.SetTracerProvider(previous)
	}()
	opts, err := projectServeOptions(state, os.Getenv, os.Environ())
	if err != nil {
		return err
	}
	opts.TracerProvider = provider
	cp, err := controlplane.New(opts)
	if err != nil {
		return err
	}
	// Close drains owned work before releasing the store writer lock.
	defer func() { err = errors.Join(err, cp.Close()) }()

	listener, err := runtime.listen("tcp", state.Address())
	if err != nil {
		return fmt.Errorf("project port is occupied: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	team, err := runtime.teamDependencies(ctx)
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	management, err := project.NewServer(state, stop, team)
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	application := http.NewServeMux()
	application.Handle("GET /views", controlplane.ViewsPage())
	application.Handle("/", controlplane.NewHTTPHandler(cp, management.Authenticate))
	server := &http.Server{Handler: management.Handler(application), ReadHeaderTimeout: 10 * time.Second}
	starterProject := state.Record.Directory
	if opts.ResolveVerification == nil {
		starterProject = ""
	}
	writeServeStatus(os.Stderr, listener.Addr().(*net.TCPAddr), opts, starterProject)
	return serveProjectHTTP(ctx, server, &ownedProjectListener{Listener: listener, management: management})
}

// Close irreversibly revokes management access and withdraws its record before
// closing the real listener, including on a failed withdrawal. The result is stable.
func (l *ownedProjectListener) Close() error {
	l.once.Do(func() {
		revokeErr := l.management.RevokeManagement()
		l.err = errors.Join(revokeErr, l.Listener.Close())
	})
	return l.err
}

// serveProjectHTTP publishes management access only for an owned listener and drains
// accepted requests during ordered shutdown so the authorized Stop response can finish.
func serveProjectHTTP(
	ctx context.Context,
	server *http.Server,
	listener *ownedProjectListener,
) (err error) {
	defer func() { err = errors.Join(err, listener.Close()) }()
	if err := listener.management.PublishRunning(os.Getpid(), os.Getenv("HC_TERMINAL_ID")); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	var serveErr error
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(serveErr, server.Shutdown(shutdownCtx))
}

// parseProjectArgs accepts no caller-selected server options; serve is an internal entrypoint.
func parseProjectArgs(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	return nil
}

// discover uses physical folders and nearest records, independent of the working tree root.
func (runtime projectRuntime) discover() (*project.State, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err := runtime.operatorRoot(cwd)
	if err != nil {
		return nil, err
	}
	return project.Discover(root, cwd)
}

// projectClient uses the literal project address with a private, proxy-free transport.
// Each generated management request obtains the currently published credential.
func projectClient(state *project.State) *genproject.Client {
	client := projectHTTPClient()
	return genproject.NewClient(
		managementEndpoint(state, client, (*genprojectclient.Client).Identity),
		teamManagementEndpoint(state),
		managementEndpoint(state, client, (*genprojectclient.Client).Stop),
		managementEndpoint(state, client, (*genprojectclient.Client).AddAgent),
		managementEndpoint(state, client, (*genprojectclient.Client).RemoveAgent),
		managementEndpoint(state, client, (*genprojectclient.Client).BrowserLink),
		managementEndpoint(state, client, (*genprojectclient.Client).ExchangeBrowserLink),
	)
}

// teamManagementEndpoint gives only the team request's deadline its own concrete recovery step.
func teamManagementEndpoint(state *project.State) goa.Endpoint {
	next := managementEndpoint(state, teamHTTPClient(), (*genprojectclient.Client).EnsureTeam)
	return func(ctx context.Context, payload any) (any, error) {
		result, err := next(ctx, payload)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("project team request did not finish; check Herdr, then run surla again: %w", context.DeadlineExceeded)
		}
		return result, err
	}
}

// managementEndpoint preserves generated transport behavior without printing peer-authored
// error text. Only declared error names select fixed CLI wording; other replies name status.
// Response status belongs to this invocation's Doer, including a changed-key retry.
func managementEndpoint(
	state *project.State,
	client *http.Client,
	endpoint func(*genprojectclient.Client) goa.Endpoint,
) goa.Endpoint {
	return func(ctx context.Context, payload any) (any, error) {
		doer := &managementDoer{state: state, client: client}
		transport := genprojectclient.NewClient("http", state.Address(), doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
		result, err := endpoint(transport)(ctx, payload)
		if err == nil {
			return result, nil
		}
		if doer.localErr != nil {
			return nil, doer.localErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, managementConnectionError(ctx, err)
		}
		if doer.lastStatus == 0 {
			return nil, err
		}
		var responseErr *goahttp.ClientError
		if errors.As(err, &responseErr) && responseErr.Name == "invalid_response" {
			if responseErr.Method == "ensure_team" && doer.lastStatus == http.StatusNotFound {
				return nil, errors.New("this project’s server was started by another version; run surla stop, then surla")
			}
			return nil, goahttp.ErrInvalidResponse(responseErr.Service, responseErr.Method, doer.lastStatus, "")
		}
		var spaceConflict *genproject.TeamSpaceConflict
		if errors.As(err, &spaceConflict) {
			return nil, fmt.Errorf("%d Herdr spaces match this project; close duplicate spaces and run surla again (HTTP %d)", spaceConflict.Count, doer.lastStatus)
		}
		var serviceErr *goa.ServiceError
		if errors.As(err, &serviceErr) {
			switch serviceErr.Name {
			case "window_busy":
				return nil, fmt.Errorf("a dialog is open in Herdr; close it, then run surla again (HTTP %d)", doer.lastStatus)
			case "team_unavailable":
				return nil, fmt.Errorf("project team could not be prepared; check Herdr and run surla again (HTTP %d)", doer.lastStatus)
			case "forbidden":
				return nil, fmt.Errorf("project management operation is not permitted (HTTP %d); run surla again", doer.lastStatus)
			case "conflict":
				return nil, fmt.Errorf("project management operation conflicts with current state (HTTP %d)", doer.lastStatus)
			case "unprocessable":
				return nil, fmt.Errorf("project management request could not be applied (HTTP %d)", doer.lastStatus)
			}
		}
		return nil, fmt.Errorf("project server response could not be used (HTTP %d); run surla again", doer.lastStatus)
	}
}

// projectHTTPClient keeps management requests off proxies and refuses redirects,
// including any proxy defaults inherited from the process environment.
func projectHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return &http.Client{
		Transport:     transport,
		Timeout:       time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// teamHTTPClient gives only the bounded team operation enough time for desktop commands.
func teamHTTPClient() *http.Client {
	client := projectHTTPClient()
	client.Timeout = 50 * time.Second
	return client
}

// managementDoer reads current authority per request and allows only one retry after
// an explicit refusal and a changed credential. It never retries an unchanged credential.
type managementDoer struct {
	state  *project.State
	client *http.Client
	// lastStatus is private to one generated endpoint invocation, never shared across calls.
	lastStatus int
	// localErr retains a private-file failure without a peer-authored error wrapper.
	localErr error
}

// Do reads authority before opening a fresh connection for this invocation.
func (d *managementDoer) Do(r *http.Request) (*http.Response, error) {
	running, err := waitForRunning(r.Context(), d.state)
	if err != nil {
		d.localErr = err
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+running.ManagementCredential)
	d.lastStatus = 0
	response, err := d.client.Do(r)
	if response != nil {
		d.lastStatus = response.StatusCode
	}
	if err != nil {
		return nil, managementConnectionError(r.Context(), err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	current, readErr := d.state.ReadRunning()
	if readErr != nil || current.ManagementCredential == running.ManagementCredential {
		if readErr != nil {
			d.localErr = fmt.Errorf("project management access was refused; run surla again: %w", readErr)
		}
		return nil, errors.Join(errors.New("project management access was refused; run surla again"), d.localErr, managementConnectionError(r.Context(), response.Body.Close()))
	}
	if err := response.Body.Close(); err != nil {
		return nil, managementConnectionError(r.Context(), err)
	}
	retry := r.Clone(r.Context())
	if r.Body != nil && r.Body != http.NoBody {
		if r.GetBody == nil {
			return nil, errors.New("cannot retry management request body")
		}
		retry.Body, err = r.GetBody()
		if err != nil {
			return nil, err
		}
	}
	retry.Header.Set("Authorization", "Bearer "+current.ManagementCredential)
	d.lastStatus = 0
	response, err = d.client.Do(retry)
	if response != nil {
		d.lastStatus = response.StatusCode
	}
	if err != nil {
		return nil, managementConnectionError(r.Context(), err)
	}
	return response, err
}

// managementConnectionError keeps malformed HTTP and transport diagnostics out of
// CLI output while retaining cancellation/deadline identity without the raw peer error.
func managementConnectionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("project server request did not finish: %w", ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("project server request did not finish: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("project server request did not finish: %w", context.Canceled)
	}
	return errors.New("project server connection failed; run surla again")
}

// waitForRunning waits without network traffic while startup has not published access.
// Corrupt records fail immediately; absence ends at the caller's existing deadline.
func waitForRunning(ctx context.Context, state *project.State) (project.Running, error) {
	for {
		running, err := state.ReadRunning()
		if err == nil {
			return running, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return project.Running{}, err
		}
		select {
		case <-ctx.Done():
			return project.Running{}, fmt.Errorf("project server has not published management access; run surla again: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// errWrongProject ends attachment before any further management operation is sent.
var errWrongProject = errors.New("the saved port answered for another project")

// confirmProject verifies the typed identity before trusting a saved port's answering process.
func confirmProject(ctx context.Context, state *project.State, client *genproject.Client) error {
	identity, err := client.Identity(ctx)
	if err != nil {
		return err
	}
	if identity.ProjectID != state.Record.ID || identity.Directory != state.Record.Directory || identity.Owner != state.Record.Owner {
		return errWrongProject
	}
	return nil
}

// startProjectTerminal delegates process lifetime to a parentless Herdr terminal; Surla never detaches a process itself.
func startProjectTerminal(
	ctx context.Context,
	state *project.State,
	findHerdr func() (string, error),
) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	hc, err := findHerdr()
	if err != nil {
		return "", err
	}
	argv := []string{"run", "--json", "--no-parent", "--bg", "--label", "surla_project=" + state.Record.ID, "--cwd", state.Record.Directory, "--", self, "serve"}
	command := exec.CommandContext(ctx, hc, argv...)
	command.Stdin = nil
	var stderr strings.Builder
	command.Stderr = &stderr
	raw, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("start Herdr server terminal: %w: %s; ensure Herdr is installed and hc is available", err, strings.TrimSpace(stderr.String()))
	}
	var reply struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("decode Herdr server terminal response: %w", err)
	}
	if reply.Address == "" {
		return "", errors.New("Herdr returned no server terminal address")
	}
	return reply.Address, nil
}

// projectHerdrProgram resolves the required external terminal executable without starting it.
func projectHerdrProgram() (string, error) {
	hc, err := exec.LookPath("hc")
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", homeErr
		}
		hc = filepath.Join(home, ".local", "bin", "hc")
		if _, executableErr := exec.LookPath(hc); executableErr != nil {
			return "", errors.New("Herdr hc is required to start Surla; install it with: curl -fsSL https://releases.herdr.dev/core/install.sh | sh (https://herdr.dev/core/)")
		}
	}
	return hc, nil
}

// preflightProjectStart refuses unavailable launch effects before creating project state.
func preflightProjectStart(findHerdr func() (string, error)) error {
	if _, err := findHerdr(); err != nil {
		return err
	}
	if _, err := projectBrowserProgram(); err != nil {
		return err
	}
	return nil
}

// projectBrowserProgram resolves the required platform opener without exposing a link.
func projectBrowserProgram() (string, error) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	program, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is required to open Surla views; install it and make it available: %w", name, err)
	}
	return program, nil
}

// openProjectViews is the only launch whose argument is allowed to contain a one-use browser credential.
func openProjectViews(ctx context.Context, url string) error {
	program, err := projectBrowserProgram()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, program, url)
	return command.Run()
}

// waitProjectTerminal observes the owned launch while startup waits for authenticated readiness.
// Canceling its context reaps the hc observer without terminating the server.
func waitProjectTerminal(ctx context.Context, terminal string) (bool, error) {
	hc, err := projectHerdrProgram()
	if err != nil {
		return false, err
	}
	command := exec.CommandContext(ctx, hc, "wait", "--json", "--timeout", "40", terminal)
	command.Stdin = nil
	var stderr strings.Builder
	command.Stderr = &stderr
	raw, err := command.Output()
	if ctx.Err() != nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("observe Herdr terminal %s: %w: %s", terminal, err, strings.TrimSpace(stderr.String()))
	}
	var result struct {
		Ended bool `json:"ended"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, fmt.Errorf("decode Herdr terminal %s status: %w", terminal, err)
	}
	return result.Ended, nil
}

// removeProjectTerminal removes only a startup terminal whose end the observer confirmed.
func removeProjectTerminal(ctx context.Context, terminal string) error {
	hc, err := projectHerdrProgram()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, hc, "rm", "--json", terminal)
	command.Stdin = nil
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove failed Herdr terminal %s: %w: %s", terminal, err, strings.TrimSpace(string(output)))
	}
	return nil
}
