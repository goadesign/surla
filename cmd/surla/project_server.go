// This file composes the per-project server with its Herdr-owned lifetime.
package main

import (
	"context"
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
	"syscall"
	"time"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"
	"go.opentelemetry.io/otel"
	goahttp "goa.design/goa/v3/http"
)

// projectRuntime binds project state and process effects at the CLI composition root.
// Tests provide an owned state root and stand-ins without starting terminals or browsers.
type projectRuntime struct {
	stateRoot      func(string, ...string) (string, error)
	preflight      func() error
	startTerminal  func(context.Context, *project.State, []string) (string, error)
	openViews      func(context.Context, string) error
	waitTerminal   func(context.Context, string) (bool, error)
	removeTerminal func(context.Context, string) error
}

// productionProjectRuntime is selected only by the executable's composition root.
func productionProjectRuntime() projectRuntime {
	return projectRuntime{
		stateRoot:      defaultOperatorRoot,
		preflight:      preflightProjectStart,
		startTerminal:  startProjectTerminal,
		openViews:      openProjectViews,
		waitTerminal:   waitProjectTerminal,
		removeTerminal: removeProjectTerminal,
	}
}

// start serializes discovery and starts at most one server, then opens a one-use views link.
func (runtime projectRuntime) start(args []string, stdout io.Writer) (err error) {
	err = parseProjectArgs("surla", args)
	if err != nil {
		return err
	}
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
		id, err := runtime.startTerminal(ctx, state, args)
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

	linkCtx, stopLink := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopLink()
	link, err := client.BrowserLink(linkCtx)
	if err != nil {
		return err
	}
	if err := runtime.openViews(linkCtx, link.URL); err != nil {
		return fmt.Errorf("open project views: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "Surla is running for %s at %s\n", state.Record.Directory, state.Origin())
	return err
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
	client, err = projectClient(state)
	if err != nil {
		return nil, created, err
	}
	if err := waitForProject(ctx, state, client); err != nil {
		if inUse {
			return nil, created, fmt.Errorf("the project server is running but not answering; no second server was started: %w", err)
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
	client, err := projectClient(state)
	if err != nil {
		return err
	}
	if err := confirmProject(ctx, state, client); err != nil {
		return fmt.Errorf("project server is running but not answering: %w", err)
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

// serve is the Herdr program entry, with project identity and address derived from private state.
func (runtime projectRuntime) serve(args []string) (err error) {
	if err := parseProjectArgs("serve", args); err != nil {
		return err
	}
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
	opts, err := projectServeOptions(state, serveConfig{}, os.Getenv, os.Environ())
	if err != nil {
		return err
	}
	opts.TracerProvider = provider
	opts.BeforeStoreRelease = state.RemoveRunning
	cp, err := controlplane.New(opts)
	if err != nil {
		return err
	}
	// Close removes the running record before releasing the store writer lock.
	defer func() { err = errors.Join(err, cp.Close()) }()

	listener, err := net.Listen("tcp", state.Address())
	if err != nil {
		return fmt.Errorf("project port is occupied: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	management := project.NewServer(state, stop)
	application := http.NewServeMux()
	application.Handle("GET /views", viewsPageHandler())
	application.Handle("/", controlplane.NewHTTPHandler(cp, management.Authenticate))
	server := &http.Server{Handler: management.Handler(application), ReadHeaderTimeout: 10 * time.Second}
	if err := state.SaveRunning(project.Running{PID: os.Getpid(), Terminal: os.Getenv("HC_TERMINAL_ID")}); err != nil {
		return errors.Join(err, listener.Close())
	}
	starterProject := state.Record.Directory
	if opts.ResolveVerification == nil {
		starterProject = ""
	}
	writeServeStatus(os.Stderr, listener.Addr().(*net.TCPAddr), opts, starterProject)
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// viewsPageHandler is the fixed data-free bootstrap boundary replaced by the views implementation.
func viewsPageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := io.WriteString(w, "<!doctype html><title>Surla</title><p>Open this project's views with surla.</p>"); err != nil {
			otel.Handle(err)
		}
	})
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
	root, err := runtime.stateRoot(cwd)
	if err != nil {
		return nil, err
	}
	return project.Discover(root, cwd)
}

// projectClient uses the generated management contract, injecting its credential only into request headers.
func projectClient(state *project.State) (*genproject.Client, error) {
	token, err := state.ManagementCredential()
	if err != nil {
		return nil, err
	}
	doer := &managementDoer{token: token, client: &http.Client{Timeout: time.Second}}
	transport := genprojectclient.NewClient("http", state.Address(), doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	return genproject.NewClient(transport.Identity(), transport.Stop(), transport.AddAgent(), transport.RemoveAgent(), transport.BrowserLink(), transport.ExchangeBrowserLink()), nil
}

// managementDoer keeps the CLI credential out of URLs, arguments, labels, and output.
type managementDoer struct {
	token  string
	client *http.Client
}

func (d *managementDoer) Do(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+d.token)
	return d.client.Do(r)
}

// errWrongProject stops readiness polling before a mismatched service receives another credential.
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

// waitForProject bounds readiness polling and treats a different answering identity as a terminal failure.
func waitForProject(ctx context.Context, state *project.State, client *genproject.Client) error {
	for {
		if err := confirmProject(ctx, state, client); err == nil {
			return nil
		} else if errors.Is(err, errWrongProject) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// startProjectTerminal delegates process lifetime to a parentless Herdr terminal; Surla never detaches a process itself.
func startProjectTerminal(ctx context.Context, state *project.State, args []string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	hc, err := projectHerdrProgram()
	if err != nil {
		return "", err
	}
	argv := []string{"run", "--json", "--no-parent", "--bg", "--label", "surla_project=" + state.Record.ID, "--cwd", state.Record.Directory, "--", self, "serve"}
	argv = append(argv, args...)
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
			return "", errors.New("Herdr is required to start Surla; install Herdr and make hc available")
		}
	}
	return hc, nil
}

// preflightProjectStart refuses unavailable launch effects before creating project state.
func preflightProjectStart() error {
	if _, err := projectHerdrProgram(); err != nil {
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
