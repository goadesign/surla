// Package harness starts the candidate orders platform for one evaluation
// scenario and drives it through its public API.
//
// The evaluator never imports or directly executes candidate code. It starts
// the candidate's digest-pinned binaries only through Surla's
// verify/candidate.Exec, which runs each one as a single sandboxed process: an
// explicit environment, a private state directory, loopback access to the
// assigned ports only, and no child processes. The harness points the
// candidate's dependency URLs at a simulator owned by the scenario and talks to
// it only with clients generated from the evaluator's own copy of the approved
// contracts. Bearer tokens come from a test identity issuer whose key is
// generated per environment; no real identity provider key is involved.
//
// The Temporal dev server each platform uses is trusted test infrastructure and
// runs outside the sandbox.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	genordersclient "github.com/goadesign/surla/examples/orders-eval/gen/http/orders/client"
	genorders "github.com/goadesign/surla/examples/orders-eval/gen/orders"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/golang-jwt/jwt/v5"
	goahttp "goa.design/goa/v3/http"
)

type (
	// Env is the shared environment of one evaluation run: the candidate's
	// pinned binaries and the test identity issuer's key. Every platform it
	// starts gets its own Temporal dev server, because the candidate's task
	// queues are fixed by its design and would otherwise be shared between
	// scenarios.
	Env struct {
		// Orders is the candidate's orders API binary.
		Orders candidate.Artifact
		// Worker is the candidate's refund worker binary.
		Worker candidate.Artifact
		// signingKey signs test bearer tokens and is given to the candidate as
		// ORDERS_JWT_KEY. It is hex text so it is always a valid environment
		// value; both sides use its bytes as the HS256 key.
		signingKey string
	}

	// Platform is one running instance of the candidate.
	Platform struct {
		// addr is the orders API's loopback host:port.
		addr string
		// logDir holds the candidate processes' output files; it is owned by
		// the evaluator and lies outside every candidate state directory.
		logDir string
		// Client calls the candidate's public orders API.
		Client *genorders.Client
		// env is the environment the platform runs in.
		env *Env
		// workerSpec restarts the refund worker with the same configuration.
		workerSpec candidate.Spec
		// apiSpec restarts the orders API with the same configuration and
		// state directory.
		apiSpec candidate.Spec
		// worker is the current refund worker process.
		worker *candidate.Process
		// api is the orders API process.
		api *candidate.Process
		// gateway serves and measures every model call the candidate makes.
		gateway *modelGateway
		// usage is the gateway's final measurement, set by Close.
		usage Usage
		// usageFinal records that Close settled every gateway call, so usage
		// is complete.
		usageFinal bool
		// started holds every candidate process started, including replaced
		// workers, so Close can await them all.
		started []*candidate.Process
		// stateDirs lists the candidate state directories Close removes.
		stateDirs []string
		// temporal is the platform's Temporal dev server.
		temporal *exec.Cmd
		// temporalDone is closed once the Temporal server exited.
		temporalDone chan struct{}
		// stopTemporal stops the Temporal server.
		stopTemporal context.CancelFunc
		// closed records that Close already ran.
		closed bool
	}
)

// Environment is the dependency profile every platform runs the candidate
// with: simulated commerce and payments. The suite declares it, so reports
// name the profile that actually ran.
const Environment = "sim"

// maxExcerptBytes bounds how much of one process's output a diagnostic shows.
const maxExcerptBytes = 16 << 10

// liveKeyEnv names the variable a live copy's orders API reads its live key
// from; only the prompts service's override_prompt accepts it.
const liveKeyEnv = "SURLA_LIVE_TARGET_KEY"

// NewEnv returns an environment for the pinned candidate binaries with a fresh
// test identity issuer key.
func NewEnv(orders, worker candidate.Artifact) (*Env, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate test issuer key: %w", err)
	}
	return &Env{Orders: orders, Worker: worker, signingKey: hex.EncodeToString(key)}, nil
}

// Usage returns the token usage of every model call the candidate made. It is
// final only after Close stopped the candidate and settled every gateway call;
// before that, or if a call never settled, it is an error.
func (p *Platform) Usage() (Usage, error) {
	if !p.usageFinal {
		return Usage{}, errors.New("model usage is not final: the platform is still running or a gateway call never settled")
	}
	return p.usage, nil
}

// Token signs a test bearer token for the tenant, subject and space-separated
// scopes.
func (e *Env) Token(tenant, subject, scope string) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": subject, "tenant_id": tenant, "scope": scope, "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(e.signingKey))
}

// Start launches a Temporal dev server and, in the sandbox, a candidate orders
// API and refund worker whose dependencies point at depsURL; it waits until the
// API accepts connections. The caller must Close the platform on every path;
// Start closes whatever it already started when it fails.
func (e *Env) Start(ctx context.Context, depsURL string) (*Platform, error) {
	return e.start(ctx, depsURL, "")
}

// StartLive starts the platform the way Start does for a live copy that
// Surla's start_live_target runs: liveKey is added to the orders API's
// environment, and nothing else, so Surla's draft_override can change its
// prompts. The model is still the evaluator's scripted model.
func (e *Env) StartLive(ctx context.Context, depsURL, liveKey string) (*Platform, error) {
	if liveKey == "" {
		return nil, errors.New("a live copy needs the key Surla minted for it")
	}
	return e.start(ctx, depsURL, liveKey)
}

// URL returns the orders API's loopback HTTP origin.
func (p *Platform) URL() string {
	return "http://" + p.addr
}

// Exited is closed once the orders API or the refund worker it started with
// exits, for whatever reason.
func (p *Platform) Exited() <-chan struct{} {
	exited := make(chan struct{})
	api, worker := p.api.Done(), p.worker.Done()
	go func() {
		select {
		case <-api:
		case <-worker:
		}
		close(exited)
	}()
	return exited
}

// start creates a platform and brings it up, closing whatever it started
// when that fails. liveKey, when set, is the orders API's live key.
func (e *Env) start(ctx context.Context, depsURL, liveKey string) (*Platform, error) {
	p := &Platform{env: e}
	if err := p.start(ctx, depsURL, liveKey); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return p, nil
}

// start brings up the platform's processes, recording each one for Close.
func (p *Platform) start(ctx context.Context, depsURL, liveKey string) error {
	e := p.env
	depsPort, err := urlPort(depsURL)
	if err != nil {
		return err
	}
	if p.logDir, err = os.MkdirTemp("", "orders-eval-logs-*"); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	temporalAddr, temporalPort, err := p.startTemporal(ctx)
	if err != nil {
		return err
	}
	if p.gateway, err = startGateway(); err != nil {
		return err
	}
	workerState, err := p.newStateDir()
	if err != nil {
		return err
	}
	p.workerSpec = candidate.Spec{
		Args:         []string{"--env", Environment, "--payments-url", depsURL, "--temporal", temporalAddr},
		StateDir:     workerState,
		ConnectPorts: []int{temporalPort, depsPort},
		LogFile:      filepath.Join(p.logDir, "worker.log"),
	}
	if p.worker, err = p.exec(ctx, e.Worker, p.workerSpec); err != nil {
		return fmt.Errorf("start refund worker: %w", err)
	}
	apiPort, err := freePort()
	if err != nil {
		return err
	}
	apiState, err := p.newStateDir()
	if err != nil {
		return err
	}
	addr := "127.0.0.1:" + strconv.Itoa(apiPort)
	p.addr = addr
	apiEnv := []string{"ORDERS_JWT_KEY=" + e.signingKey}
	if liveKey != "" {
		apiEnv = append(apiEnv, liveKeyEnv+"="+liveKey)
	}
	p.apiSpec = candidate.Spec{
		Args: []string{"--env", Environment, "--commerce-url", depsURL, "--temporal", temporalAddr,
			"--listen", addr, "--model-provider", "gateway", "--model-gateway", p.gateway.addr,
			"--state-dir", apiState},
		Env:          apiEnv,
		StateDir:     apiState,
		ConnectPorts: []int{temporalPort, depsPort, p.gateway.port},
		ListenPort:   apiPort,
		LogFile:      filepath.Join(p.logDir, "orders.log"),
	}
	if p.api, err = p.exec(ctx, e.Orders, p.apiSpec); err != nil {
		return fmt.Errorf("start orders API: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := WaitListening(waitCtx, addr); err != nil {
		return fmt.Errorf("orders API never listened: %w\n%s", err, excerpt("orders", p.api))
	}
	c := genordersclient.NewClient("http", addr, http.DefaultClient, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	p.Client = genorders.NewClient(c.Chat(), c.RunStatus(), c.ApproveRefund())
	return nil
}

// Close kills every candidate process the platform started, stops its model
// gateway once every call in flight returned, stops its Temporal server, waits
// until all of them exited, and then removes the candidate state directories. Logs are kept as evidence. Calling Close again
// does nothing.
func (p *Platform) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	for _, proc := range p.started {
		errs = append(errs, proc.Kill())
	}
	if p.gateway != nil {
		err := p.gateway.stop()
		errs = append(errs, err)
		p.usage, p.usageFinal = p.gateway.meter.total(), err == nil
	}
	if p.stopTemporal != nil {
		p.stopTemporal()
		<-p.temporalDone
	}
	for _, dir := range p.stateDirs {
		errs = append(errs, os.RemoveAll(dir))
	}
	return errors.Join(errs...)
}

// KillWorker sends SIGKILL to the refund worker and waits for it to exit.
func (p *Platform) KillWorker() error {
	return p.worker.Kill()
}

// RestartWorker starts a replacement refund worker with the same configuration,
// logging to a new file so the crashed worker's output is kept.
func (p *Platform) RestartWorker(ctx context.Context) error {
	spec := p.workerSpec
	spec.LogFile = filepath.Join(p.logDir, "worker-restarted.log")
	w, err := p.exec(ctx, p.env.Worker, spec)
	if err != nil {
		return fmt.Errorf("restart refund worker: %w", err)
	}
	p.worker = w
	return nil
}

// RestartAPI kills the orders API with SIGKILL and starts a replacement on
// the same port and state directory, so the replacement must recover runs and
// pending approvals from the durable store. It waits until the replacement
// accepts connections.
func (p *Platform) RestartAPI(ctx context.Context) error {
	if err := p.api.Kill(); err != nil {
		return err
	}
	spec := p.apiSpec
	spec.LogFile = filepath.Join(p.logDir, "orders-restarted.log")
	api, err := p.exec(ctx, p.env.Orders, spec)
	if err != nil {
		return fmt.Errorf("restart orders API: %w", err)
	}
	p.api = api
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := WaitListening(waitCtx, "127.0.0.1:"+strconv.Itoa(spec.ListenPort)); err != nil {
		return fmt.Errorf("restarted orders API never listened: %w\n%s", err, excerpt("orders", p.api))
	}
	return nil
}

// Logs returns bounded excerpts of the candidate processes' output for
// diagnostics; full output stays in the evaluator-owned log directory.
func (p *Platform) Logs() string {
	return excerpt("orders", p.api) + "\n" + excerpt("worker", p.worker) + "\nfull logs: " + p.logDir
}

// WaitStatus polls a run until it reaches want or a terminal status, and
// returns its last state. It reports an error only when the API cannot be
// read or ctx ends first.
func (p *Platform) WaitStatus(ctx context.Context, token, runID, want string) (*genorders.RunView, error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		v, err := p.Client.RunStatus(ctx, &genorders.RunStatusPayload{Token: token, RunID: runID})
		if err != nil {
			return nil, fmt.Errorf("read run %s: %w", runID, err)
		}
		if v.Status == want || v.Status == "completed" || v.Status == "failed" {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, fmt.Errorf("run %s still %s: %w", runID, v.Status, ctx.Err())
		case <-tick.C:
		}
	}
}

// WaitListening waits until addr accepts TCP connections or ctx ends.
func WaitListening(ctx context.Context, addr string) error {
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// excerpt renders the end of one process's output, noting truncation.
func excerpt(name string, p *candidate.Process) string {
	text, truncated, err := p.Excerpt(maxExcerptBytes)
	if err != nil {
		return name + ": output unavailable: " + err.Error()
	}
	if truncated {
		return name + " (last " + strconv.Itoa(maxExcerptBytes) + " bytes):\n" + text
	}
	return name + ":\n" + text
}

// exec starts a sandboxed candidate process and records it for Close.
func (p *Platform) exec(ctx context.Context, a candidate.Artifact, spec candidate.Spec) (*candidate.Process, error) {
	proc, err := candidate.Exec(ctx, a, spec)
	if err != nil {
		return nil, err
	}
	p.started = append(p.started, proc)
	return proc, nil
}

// newStateDir creates a private candidate state directory recorded for Close.
func (p *Platform) newStateDir() (string, error) {
	dir, err := os.MkdirTemp("", "orders-eval-state-*")
	if err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	p.stateDirs = append(p.stateDirs, dir)
	return dir, nil
}

// startTemporal starts the platform's in-memory Temporal dev server and
// returns its frontend address and port once it listens. The server's
// lifetime belongs to the platform and ends in Close; ctx bounds only the wait
// for it to listen, so a canceled scenario stops waiting at once.
func (p *Platform) startTemporal(ctx context.Context) (string, int, error) {
	port, err := freePort()
	if err != nil {
		return "", 0, err
	}
	uiPort, err := freePort()
	if err != nil {
		return "", 0, err
	}
	logPath := filepath.Join(p.logDir, "temporal.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create temporal log: %w", err)
	}
	defer logFile.Close()
	serverCtx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(serverCtx, "temporal", "server", "start-dev", "--headless",
		"--ip", "127.0.0.1", "--port", strconv.Itoa(port), "--ui-port", strconv.Itoa(uiPort), "--log-level", "error")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		stop()
		return "", 0, fmt.Errorf("start temporal: %w", err)
	}
	p.temporal, p.stopTemporal, p.temporalDone = cmd, stop, make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(p.temporalDone)
	}()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := WaitListening(waitCtx, addr); err != nil {
		return "", 0, fmt.Errorf("temporal never listened: %w\n%s", err, tailFile(logPath, maxExcerptBytes))
	}
	return addr, port, nil
}

// urlPort returns the TCP port of an http URL.
func urlPort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", raw, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0, fmt.Errorf("%s has no numeric port: %w", raw, err)
	}
	return port, nil
}

// freePort returns a currently free loopback port.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}

// tailFile returns at most the last maxBytes of a trusted helper's log file
// for a diagnostic, naming the file that holds the full output.
func tailFile(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "output unavailable: " + err.Error()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "output unavailable: " + err.Error()
	}
	start := max(info.Size()-maxBytes, 0)
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return "output unavailable: " + err.Error()
	}
	return "last " + strconv.FormatInt(int64(len(b)), 10) + " bytes of " + path + ":\n" + string(b)
}
