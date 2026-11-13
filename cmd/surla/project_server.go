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
	stateRoot     func(string, ...string) (string, error)
	startTerminal func(context.Context, *project.State, []string) error
	openViews     func(context.Context, string) error
}

// productionProjectRuntime is selected only by the executable's composition root.
func productionProjectRuntime() projectRuntime {
	return projectRuntime{
		stateRoot:     defaultOperatorRoot,
		startTerminal: startProjectTerminal,
		openViews:     openProjectViews,
	}
}

// start serializes discovery and starts at most one server, then opens a one-use views link.
func (runtime projectRuntime) start(args []string, stdout io.Writer) (err error) {
	_, err = parseProjectFlags("surla", args)
	if err != nil {
		return err
	}
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, created, err := attachOrStartProject(ctx, state, func(ctx context.Context, state *project.State) error { return runtime.startTerminal(ctx, state, args) })
	if err != nil {
		return err
	}
	if created {
		if err := writeProjectCreatedNotice(stdout); err != nil {
			return err
		}
	}

	link, err := client.BrowserLink(ctx)
	if err != nil {
		return err
	}
	if err := runtime.openViews(ctx, link.URL); err != nil {
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
	launch func(context.Context, *project.State) error,
) (client *genproject.Client, created bool, err error) {
	lock, err := state.StartLock(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	created, err = state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
	if err != nil {
		return nil, false, err
	}
	inUse, err := controlplane.CardStoreInUse(state.CardsDir())
	if err != nil {
		return nil, false, err
	}
	if !inUse {
		listener, err := net.Listen("tcp", state.Address())
		if err != nil {
			return nil, false, fmt.Errorf("project port %d is occupied; no server was started: %w", state.Record.Port, err)
		}
		if err := listener.Close(); err != nil {
			return nil, false, err
		}
		if err := launch(ctx, state); err != nil {
			return nil, false, err
		}
	}
	client, err = projectClient(state)
	if err != nil {
		return nil, false, err
	}
	if err := waitForProject(ctx, state, client); err != nil {
		if inUse {
			return nil, false, fmt.Errorf("the project server is running but not answering; no second server was started: %w", err)
		}
		return nil, false, fmt.Errorf("the project server did not start answering: %w", err)
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
	cfg, err := parseProjectFlags("serve", args)
	if err != nil {
		return err
	}
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	if state.Record.ID == "" {
		return errors.New("start the project with surla")
	}
	cfg.projectDir = state.Record.Directory
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
	opts, err := runtime.serveOptions(cfg, os.Getenv, os.Environ())
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
	starterProject := cfg.projectDir
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

// parseProjectFlags preserves supported execution configuration while deleting caller-selected identity and stores.
func parseProjectFlags(name string, args []string) (serveConfig, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var cfg serveConfig
	bindServeConfigFlags(fs, &cfg)
	if err := parseServeConfigFlags(fs, args); err != nil {
		return serveConfig{}, err
	}
	return cfg, nil
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

// confirmProject verifies the typed identity before trusting a saved port's answering process.
func confirmProject(ctx context.Context, state *project.State, client *genproject.Client) error {
	identity, err := client.Identity(ctx)
	if err != nil {
		return err
	}
	if identity.ProjectID != state.Record.ID || identity.Directory != state.Record.Directory || identity.Owner != state.Record.Owner {
		return errors.New("the saved port answered for another project")
	}
	return nil
}

// waitForProject bounds readiness polling and treats a different answering identity as a terminal failure.
func waitForProject(ctx context.Context, state *project.State, client *genproject.Client) error {
	for {
		if err := confirmProject(ctx, state, client); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// startProjectTerminal delegates process lifetime to a parentless Herdr terminal; Surla never detaches a process itself.
func startProjectTerminal(ctx context.Context, state *project.State, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	hc, err := exec.LookPath("hc")
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return homeErr
		}
		hc = filepath.Join(home, ".local", "bin", "hc")
		if _, statErr := os.Stat(hc); statErr != nil {
			return errors.New("Herdr is required to start Surla; install Herdr and make hc available")
		}
	}
	argv := []string{"run", "--json", "--no-parent", "--bg", "--label", "surla_project=" + state.Record.ID, "--cwd", state.Record.Directory, "--", self, "serve"}
	argv = append(argv, args...)
	command := exec.CommandContext(ctx, hc, argv...)
	command.Stdin = nil
	// hc returns terminal metadata only; no credential is present in its arguments or response.
	raw, err := command.Output()
	if err != nil {
		return fmt.Errorf("start Herdr server terminal: %w", err)
	}
	var reply json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("decode Herdr server terminal response: %w", err)
	}
	return nil
}

// openProjectViews is the only launch whose argument is allowed to contain a one-use browser credential.
func openProjectViews(ctx context.Context, url string) error {
	program := "xdg-open"
	if runtime.GOOS == "darwin" {
		program = "open"
	}
	command := exec.CommandContext(ctx, program, url)
	return command.Run()
}
