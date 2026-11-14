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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	genprojectclient "github.com/goadesign/surla/gen/http/project/client"
	gensurlaclient "github.com/goadesign/surla/gen/http/surla/client"
	genproject "github.com/goadesign/surla/gen/project"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/modelsettings"
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
	readTerminal   func(context.Context, string) (string, error) // an ended terminal's text, before removal
	removeTerminal func(context.Context, string) error
	// listen binds only the saved project address; tests observe the actual close boundary.
	listen func(string, string) (net.Listener, error)
	// teamDependencies binds the server login environment and accepted-work lifetime; its
	// []string is the server's original environment, which only Herdr's programs receive.
	teamDependencies func(context.Context, []string) (project.TeamDependencies, error)
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

const (
	// startOutputLines bounds how many of an ended server's last lines a start error shows:
	// its final error, including a few joined lines, and what led to it, in one screen.
	startOutputLines = 20
	// startOutputBytes bounds the same lines in bytes so one runaway line cannot flood the
	// user's terminal, while a long final error still fits whole.
	startOutputBytes = 4096
)

// productionProjectRuntime is selected only by the executable's composition root.
func productionProjectRuntime() projectRuntime {
	return projectRuntime{
		configRoot: os.UserConfigDir,
		preflight: func() error {
			commands, err := cliTeamCommands()
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
			commands, err := cliTeamCommands()
			if err != nil {
				return err
			}
			return startTeamPrograms(ctx, commands)
		},
		startTerminal: func(ctx context.Context, state *project.State) (string, error) {
			return startProjectTerminal(ctx, state, projectHerdrProgram)
		},
		readTerminal: func(ctx context.Context, terminal string) (string, error) {
			return readProjectTerminal(ctx, terminal, projectHerdrProgram)
		},
		openViews:      openProjectViews,
		waitTerminal:   waitProjectTerminal,
		removeTerminal: removeProjectTerminal,
		listen:         net.Listen,
	}
}

// cliTeamCommands resolves the team programs the surla command runs. Herdr's programs get
// this command's environment unchanged: a Herdr core one of them starts keeps it for every
// terminal, and agents may sign in with its provider variables. Chorus gets it without any
// model provider family.
func cliTeamCommands() (teamCommands, error) {
	commands, err := resolveTeamCommands(exec.LookPath, runTeamCommand, installerProgramFolder())
	if err != nil {
		return teamCommands{}, err
	}
	commands.start = startDetachedTeamProgram
	commands.herdrEnv = os.Environ()
	commands.chorusEnv = withoutProviderFamilies(os.Environ())
	return commands, nil
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
				// The ended terminal holds the server's own error until it is removed.
				// The read and the removal each get their own deadline: a read that
				// hangs must not leave the removal with one that has already passed.
				readCtx, stopRead := context.WithTimeout(context.Background(), 5*time.Second)
				output, readErr := runtime.readTerminal(readCtx, terminal)
				stopRead()
				err = endedStartError(err, output, readErr)
				removeCtx, stopRemove := context.WithTimeout(context.Background(), 5*time.Second)
				err = errors.Join(err, runtime.removeTerminal(removeCtx, terminal))
				stopRemove()
			}
		}
	}
	if created {
		err = errors.Join(err, writeProjectCreatedNotice(stdout))
	}
	if err != nil {
		return err
	}

	// The server answered for this project, so this shell's model provider
	// settings go to it before anything else; a refusal stops the start.
	settingsCtx, stopSettings := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopSettings()
	if err := handOverModelSettings(settingsCtx, state, modelsettings.Select(os.Environ()), stdout); err != nil {
		return err
	}

	// Confirming and approving on the views page need no agent, so a team that cannot
	// be prepared is reported after the page opens, and still fails the command.
	teamErr := runtime.startTeamPrograms(context.Background())
	if teamErr == nil {
		teamCtx, stopTeam := context.WithTimeout(context.Background(), 50*time.Second)
		defer stopTeam()
		team, err := client.EnsureTeam(teamCtx)
		if err != nil {
			teamErr = err
		} else if _, err := fmt.Fprintf(stdout, "Team %s.\n%s\n", team.Team, team.Step); err != nil {
			return err
		}
	}
	if teamErr != nil {
		teamErr = fmt.Errorf("the project team is not ready: %w", teamErr)
	}

	linkCtx, stopLink := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopLink()
	link, err := client.BrowserLink(linkCtx)
	if err != nil {
		return errors.Join(err, teamErr)
	}
	if err := validateProjectViewsLink(state, link.URL); err != nil {
		return errors.Join(err, teamErr)
	}
	if err := runtime.openViews(linkCtx, link.URL); err != nil {
		return errors.Join(fmt.Errorf("open project views: %w", err), teamErr)
	}
	_, err = fmt.Fprintf(stdout, "Surla is running for %s at %s\n", state.Record.Directory, state.Origin())
	return errors.Join(err, teamErr)
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
			return nil, created, takenPortError(state, err)
		}
		if err := listener.Close(); err != nil {
			return nil, created, err
		}
		if terminal, err = launch(ctx, state); err != nil {
			return nil, created, err
		}
	}
	client = projectClient(state)
	identity, err := confirmProject(ctx, state, client)
	if err != nil {
		if inUse {
			return nil, created, fmt.Errorf("%s; no second server was started: %w", projectHolder(state), err)
		}
		return nil, created, fmt.Errorf("the project server in Herdr terminal %s did not start answering: %w", terminal, err)
	}
	if err := confirmBuild(identity); err != nil {
		return nil, created, err
	}
	return client, created, nil
}

// takenPortError names what holds a stopped project's saved port and the step that frees it.
// The holder is another project's server when a project sharing the port, saved before new
// projects avoided each other's ports, holds its card store; otherwise it is another program.
// The saved port is kept: agents' connections and the launcher roster name it.
func takenPortError(state *project.State, listenErr error) error {
	port := state.Record.Port
	if !errors.Is(listenErr, syscall.EADDRINUSE) {
		return fmt.Errorf("project port %d could not be used; no server was started: %w", port, listenErr)
	}
	sharing, err := state.ProjectsSharingPort()
	var checkErrs []error
	if err != nil {
		checkErrs = append(checkErrs, err)
	}
	for _, other := range sharing {
		inUse, err := controlplane.CardStoreInUse(other.CardsDir())
		if err != nil {
			checkErrs = append(checkErrs, err)
			continue
		}
		if inUse {
			return fmt.Errorf("project port %d is held by the Surla server of the project in %s, which saved the same port, so only one of the two can run at a time; no server was started; run surla stop in that folder, then run surla here", port, other.Record.Directory)
		}
	}
	taken := fmt.Errorf("another program is listening on %s, the address saved for this project; no server was started; stop that program, then run surla (lsof -nP -iTCP:%d -sTCP:LISTEN names it)", state.Address(), port)
	if len(checkErrs) > 0 {
		return errors.Join(taken, fmt.Errorf("other Surla projects could not be checked for this port: %w", errors.Join(checkErrs...)))
	}
	return taken
}

// endedStartError explains a start whose owned terminal ended before the server answered.
// The server's own last lines replace the attach failure, which then only reports the end's
// consequence; without readable lines, the attach failure stays and says why none are shown.
func endedStartError(attachErr error, output string, readErr error) error {
	if readErr != nil {
		return errors.Join(attachErr, fmt.Errorf("the project server's terminal output could not be read: %w", readErr))
	}
	lines := lastServerLines(output)
	if lines == "" {
		return errors.Join(attachErr, errors.New("the project server's terminal output could not be read: it showed no printable text"))
	}
	return fmt.Errorf("the project server stopped while starting, and its last lines were:\n%s", lines)
}

// lastServerLines keeps the end of a server terminal's text within startOutputLines and
// startOutputBytes, as printable text indented under the CLI's sentence. Blank rows around
// the text are dropped; a cut long line starts with an ellipsis. Empty means nothing to show.
func lastServerLines(output string) string {
	lines := strings.Split(printableTerminalText(output), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	lines = lines[len(lines)-min(len(lines), startOutputLines):]
	text := strings.TrimLeft(strings.Join(lines, "\n"), "\n")
	if len(text) > startOutputBytes {
		text = text[len(text)-startOutputBytes:]
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = strings.TrimLeft(text[newline+1:], "\n")
		} else {
			for len(text) > 0 && !utf8.RuneStart(text[0]) {
				text = text[1:]
			}
			text = "…" + text
		}
	}
	if text == "" {
		return ""
	}
	lines = strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "  " + line
		}
	}
	return strings.Join(lines, "\n")
}

// printableTerminalText keeps what a terminal shows as text: escape sequences, control
// characters and invalid bytes are dropped, newlines are kept and tabs become spaces.
func printableTerminalText(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		switch {
		case r == '\x1b':
			i = skipEscapeSequence(text, i)
		case r == '\n':
			out.WriteByte('\n')
		case r == '\t':
			out.WriteByte(' ')
		case r == utf8.RuneError && size == 1:
			// An invalid byte is not text and is dropped.
		case unicode.IsPrint(r):
			out.WriteRune(r)
		}
	}
	return out.String()
}

// skipEscapeSequence returns the index just past the escape sequence whose ESC precedes
// index i: a control sequence (ESC [), a control string ended by BEL or ESC \ (ESC ] P X ^ _),
// or ESC with intermediates and a final byte. A newline always ends a sequence and is kept.
func skipEscapeSequence(text string, i int) int {
	if i >= len(text) {
		return i
	}
	switch text[i] {
	case '[':
		for i++; i < len(text) && text[i] >= 0x20 && text[i] <= 0x3f; i++ {
		}
		if i < len(text) && text[i] >= 0x40 && text[i] <= 0x7e {
			i++
		}
		return i
	case ']', 'P', 'X', '^', '_':
		for i++; i < len(text); i++ {
			switch {
			case text[i] == '\a':
				return i + 1
			case text[i] == '\x1b' && i+1 < len(text) && text[i+1] == '\\':
				return i + 2
			case text[i] == '\x1b', text[i] == '\n':
				return i
			}
		}
		return i
	}
	for i < len(text) && text[i] >= 0x20 && text[i] <= 0x2f {
		i++
	}
	if i < len(text) && text[i] >= 0x30 && text[i] <= 0x7e {
		i++
	}
	return i
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
	if _, err := confirmProject(ctx, state, client); err != nil {
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

// serve is the Herdr program entry. stateDir is the private state folder the
// launching `surla` found and passed with -state; project identity and address
// come from the record there, and serve never derives a folder from its own
// environment or working directory.
func (runtime projectRuntime) serve(stateDir string) (err error) {
	// Provider settings reach the server only through hand_over_model_settings: none that
	// Herdr's core environment holds may supply a live target, a verification or a check.
	herdrEnv, err := takeProviderFamilies()
	if err != nil {
		return err
	}
	state, err := project.Open(stateDir)
	if err != nil {
		return err
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
	opts, err := projectServeOptions(state, os.Environ())
	if err != nil {
		return err
	}
	opts.TracerProvider = provider
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	team, err := runtime.teamDependencies(ctx, herdrEnv)
	if err != nil {
		return err
	}
	management, err := project.NewServer(state, stop, team)
	if err != nil {
		return err
	}
	opts.Team = &controlplane.TeamBinding{Owner: state.Record.Owner, Service: management}
	cp, err := controlplane.New(opts)
	if err != nil {
		return err
	}
	// Close drains owned work before releasing the store writer lock.
	defer func() { err = errors.Join(err, cp.Close()) }()

	listener, err := runtime.listen("tcp", state.Address())
	if err != nil {
		return fmt.Errorf("project port %d is occupied: %w; stop the program listening on it, then run surla", state.Record.Port, err)
	}
	application := http.NewServeMux()
	application.Handle("GET /views", controlplane.ViewsPage())
	application.Handle("/", controlplane.NewHTTPHandler(cp, management.Authenticate))
	server := &http.Server{Handler: management.Handler(application), ReadHeaderTimeout: 10 * time.Second}
	writeServeStatus(os.Stderr, listener.Addr().(*net.TCPAddr), opts)
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

// parseServeArgs returns the state folder named by serve's one required flag,
// -state, which only startProjectTerminal supplies; serve is an internal
// entrypoint and accepts no other option or argument.
func parseServeArgs(args []string) (string, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	stateDir := fs.String("state", "", "private project state folder, passed by surla when it starts the server")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if *stateDir == "" {
		return "", errors.New("serve requires -state; start the project with surla")
	}
	return *stateDir, nil
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
			return nil, fmt.Errorf("project team request did not finish because Herdr did not answer in time; open the Herdr app, then click Start agents on the views page: %w", context.DeadlineExceeded)
		}
		return result, err
	}
}

// managementEndpoint preserves generated transport behavior without printing peer-authored
// error text for one project service operation (see managementCall).
func managementEndpoint(
	state *project.State,
	client *http.Client,
	endpoint func(*genprojectclient.Client) goa.Endpoint,
) goa.Endpoint {
	return managementCall(state, client, func(doer goahttp.Doer) goa.Endpoint {
		return endpoint(genprojectclient.NewClient("http", state.Address(), doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false))
	})
}

// modelSettingsEndpoint is the management call of the surla service's
// hand_over_model_settings, with the same credential, retry and wording as
// the project service's management calls.
func modelSettingsEndpoint(state *project.State) goa.Endpoint {
	return managementCall(state, projectHTTPClient(), func(doer goahttp.Doer) goa.Endpoint {
		return gensurlaclient.NewClient("http", state.Address(), doer, goahttp.RequestEncoder, goahttp.ResponseDecoder, false).HandOverModelSettings()
	})
}

// managementCall preserves generated transport behavior without printing peer-authored
// error text. endpoint builds the generated operation on a fresh Doer for each invocation.
// Only declared error names select fixed CLI wording; other replies name status.
// Response status belongs to this invocation's Doer, including a changed-key retry.
func managementCall(
	state *project.State,
	client *http.Client,
	endpoint func(goahttp.Doer) goa.Endpoint,
) goa.Endpoint {
	return func(ctx context.Context, payload any) (any, error) {
		doer := &managementDoer{state: state, client: client}
		result, err := endpoint(doer)(ctx, payload)
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
			newOperation := responseErr.Method == "ensure_team" || responseErr.Method == "hand_over_model_settings"
			if newOperation && doer.lastStatus == http.StatusNotFound {
				return nil, errors.New("this project’s server was started by another version; run surla stop, then surla")
			}
			if responseErr.Method == "hand_over_model_settings" && doer.lastStatus == http.StatusBadRequest {
				return nil, fmt.Errorf("the project server refused the model settings from this shell (HTTP %d); "+
					"each ANTHROPIC_ and OPENAI_ request setting must be at most %d characters", doer.lastStatus, modelsettings.MaxValueLength)
			}
			return nil, goahttp.ErrInvalidResponse(responseErr.Service, responseErr.Method, doer.lastStatus, "")
		}
		var spaceConflict *genproject.TeamSpaceConflict
		if errors.As(err, &spaceConflict) {
			return nil, fmt.Errorf("%d Herdr spaces match this project; close duplicate spaces and run surla again (HTTP %d)", spaceConflict.Count, doer.lastStatus)
		}
		var unavailable *genproject.TeamUnavailable
		if errors.As(err, &unavailable) {
			return nil, fmt.Errorf("%s (HTTP %d)", teamUnavailableStep(state, unavailable.Cause), doer.lastStatus)
		}
		var serviceErr *goa.ServiceError
		if errors.As(err, &serviceErr) {
			switch serviceErr.Name {
			case "window_busy":
				return nil, fmt.Errorf("a dialog is open in Herdr; close it, then run surla again (HTTP %d)", doer.lastStatus)
			case "forbidden":
				return nil, fmt.Errorf("project management operation is not permitted (HTTP %d); run surla again", doer.lastStatus)
			case "unprocessable":
				return nil, fmt.Errorf("project management request could not be applied (HTTP %d)", doer.lastStatus)
			}
		}
		return nil, fmt.Errorf("project server response could not be used (HTTP %d); run surla again", doer.lastStatus)
	}
}

// teamUnavailableStep is the CLI's fixed text for one declared team failure cause. It names
// what could not be used and the step that repairs it; the retry is the views page's
// Start agents, which reaches the same team operation, because the page opens regardless.
func teamUnavailableStep(state *project.State, cause string) string {
	const retry = "then click Start agents on the views page"
	switch cause {
	case "stopping":
		return "the project server is stopping; run surla again"
	case "herdr":
		return "Herdr did not complete a team command; open the Herdr app, " + retry
	case "roster_folder":
		return "Herdr's launcher roster folder could not be written; make data/chorus-launcher/rosters in Herdr's app folder (~/.config/herdr-app unless HERDR_APP_HOME is set) writable by this account, " + retry
	case "project_state":
		return fmt.Sprintf("this project's private state in %s could not be read or written; make that folder and the folders in it private to this account (chmod 700) and its files too (chmod 600), %s", state.Dir, retry)
	case "role_folder":
		return fmt.Sprintf("a team role's working folder could not be read; make sure this account can open the project folder %s and its build working folders, %s", state.Record.Directory, retry)
	}
	return "project team could not be prepared; run surla again"
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

// errOtherBuild refuses to attach to a server another surla build started, such as the
// one still running after an upgrade: it would serve its own tools and page, and start
// the new executable for its child processes. surla stop accepts any build.
var errOtherBuild = errors.New("this project's server was started by another build of surla; run surla stop, then surla")

// confirmProject verifies the typed identity before trusting a saved port's answering process.
// It accepts a server of any build, so surla stop reaches one an earlier build started.
func confirmProject(ctx context.Context, state *project.State, client *genproject.Client) (*genproject.ProjectIdentity, error) {
	identity, err := client.Identity(ctx)
	if err != nil {
		return nil, err
	}
	if identity.ProjectID != state.Record.ID || identity.Directory != state.Record.Directory || identity.Owner != state.Record.Owner {
		return nil, errWrongProject
	}
	return identity, nil
}

// confirmBuild attaches only to a server started by this build; a server that reports no
// build predates the field and is another build.
func confirmBuild(identity *genproject.ProjectIdentity) error {
	build, err := project.BuildIdentity()
	if err != nil {
		return err
	}
	if identity.Build == nil || *identity.Build != build {
		return errOtherBuild
	}
	return nil
}

// startProjectTerminal delegates process lifetime to a parentless Herdr terminal; Surla never detaches a process itself.
// The server gets the state folder this command found as -state, because a Herdr terminal runs with the
// environment of Herdr's core, not this shell's, and could otherwise resolve another configuration folder.
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
	argv := []string{"run", "--json", "--no-parent", "--bg", "--label", "surla_project=" + state.Record.ID, "--cwd", state.Record.Directory, "--", self, "serve", "-state", state.Dir}
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
// The opener never receives the shell's model provider settings.
func openProjectViews(ctx context.Context, url string) error {
	program, err := projectBrowserProgram()
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, program, url)
	command.Env = withoutProviderFamilies(os.Environ())
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

// readProjectTerminal returns the text an ended startup terminal printed, standard output and
// error in order, asking Herdr for the screen plus the history lines a start error can show.
// The terminal must still exist, so start reads it before removing it.
func readProjectTerminal(
	ctx context.Context,
	terminal string,
	findHerdr func() (string, error),
) (string, error) {
	hc, err := findHerdr()
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, hc, "read", "--json", "--lines", strconv.Itoa(startOutputLines), terminal)
	command.Stdin = nil
	var stderr strings.Builder
	command.Stderr = &stderr
	raw, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("read Herdr terminal %s: %w: %s", terminal, err, strings.TrimSpace(stderr.String()))
	}
	var reply struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("decode Herdr terminal %s output: %w", terminal, err)
	}
	return reply.Text, nil
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
