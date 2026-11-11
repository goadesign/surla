// Command orders-surla serves Surla's CP3 prompt-edit tool against a running
// orders API. It is a reference host, separate from the orders process:
// SURLA_AUTH_TOKEN authenticates MCP callers, and ORDERS_PROMPTS_TOKEN grants
// this host the target API's prompts:manage permission.
//
// Callers propose and revise Change Cards by sending their own analysis of the
// request, so this host needs no model connection. It has no CP4 verifier and
// no CP5 promotion target. Its control plane comes from the exact Surla
// version in this module's go.mod.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	genordersclient "github.com/goadesign/surla/examples/orders/gen/http/orders/client"
	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	"github.com/goadesign/surla/internal/controlplane"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goahttp "goa.design/goa/v3/http"
)

type (
	// config holds operator-chosen addresses and the two independent bearer
	// credentials. Credentials are read after flag parsing, so help never
	// prints environment secrets as defaults.
	config struct {
		// listen is the address of this host's HTTP and MCP server.
		listen string
		// principal identifies the operator authenticated by authToken.
		principal string
		// authToken authenticates callers of this host, not the orders API.
		authToken string
		// ordersScheme and ordersHost are the validated target origin. Only
		// parseConfig constructs them; the generated client owns its routes.
		ordersScheme, ordersHost string
		// ordersToken grants prompts:manage at that target API.
		ordersToken string
		// cardsDir is the directory where Change Cards are saved so they
		// survive restarts; one host at a time may use it.
		cardsDir string
	}

	// ordersPrompts implements Surla's prompt updater by calling the orders
	// API with its operator credential. It retains no prompt text or version.
	ordersPrompts struct {
		// client calls the generated orders API contract.
		client *genorders.Client
		// token grants this host permission to edit target prompts.
		token string
	}
)

// ApplyPromptOverride sends the proposed text to the authenticated target
// operation. Its returned version identifies the accepted text, not a promise
// that no later operator has replaced it.
func (p *ordersPrompts) ApplyPromptOverride(ctx context.Context, promptID, newContent string) (string, error) {
	ctx, span := otel.Tracer("orders-surla").Start(ctx, "orders.override_prompt",
		trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	res, err := p.client.OverridePrompt(ctx, &genorders.OverridePromptPayload{
		Token: p.token, PromptID: promptID, Template: newContent,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "orders prompt update failed")
		return "", err
	}
	span.AddEvent("prompt text accepted")
	return res.Version, nil
}

// main reads the operator configuration and serves until interrupted.
func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = serve(ctx, cfg, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orders-surla:", err)
		os.Exit(1)
	}
}

// parseConfig reads flags, then resolves environment credentials. Missing
// credentials or an orders URL other than an HTTP(S) origin stop startup.
func parseConfig(args []string, getenv func(string) string, output io.Writer) (config, error) {
	var cfg config
	var ordersURL string
	fs := flag.NewFlagSet("orders-surla", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.listen, "listen", "127.0.0.1:8091", "MCP and HTTP listen address")
	fs.StringVar(&cfg.principal, "principal", "", "Authenticated MCP operator ID (required)")
	fs.StringVar(&cfg.authToken, "auth-token", "", "MCP bearer token (default: $SURLA_AUTH_TOKEN)")
	fs.StringVar(&ordersURL, "orders-url", "", "Orders API origin, such as http://127.0.0.1:8080 (required)")
	fs.StringVar(&cfg.ordersToken, "orders-token", "", "Target prompts:manage bearer token (default: $ORDERS_PROMPTS_TOKEN)")
	fs.StringVar(&cfg.cardsDir, "cards-dir", ".surla/orders-surla-cards", "Directory where Change Cards are saved so they survive restarts; one host at a time may use it")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if cfg.authToken == "" {
		cfg.authToken = getenv("SURLA_AUTH_TOKEN")
	}
	if cfg.ordersToken == "" {
		cfg.ordersToken = getenv("ORDERS_PROMPTS_TOKEN")
	}
	cfg.principal = strings.TrimSpace(cfg.principal)
	cfg.authToken = strings.TrimSpace(cfg.authToken)
	cfg.ordersToken = strings.TrimSpace(cfg.ordersToken)
	if cfg.principal == "" || cfg.authToken == "" || cfg.ordersToken == "" {
		return config{}, errors.New("-principal, MCP bearer token, and target prompts:manage bearer token are required")
	}
	if !utf8.ValidString(cfg.principal) {
		return config{}, errors.New("-principal must be valid UTF-8")
	}
	origin, err := parseOrdersURL(ordersURL)
	if err != nil {
		return config{}, err
	}
	cfg.ordersScheme, cfg.ordersHost = origin.Scheme, origin.Host
	return cfg, nil
}

// serve constructs the target client and control plane, then owns the HTTP
// listener and cards directory lock until ctx ends. Shutting down this host
// does not stop orders.
func serve(ctx context.Context, cfg config, output io.Writer) (err error) {
	handler, closeCP, err := newHandler(cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeCP()) }()
	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(output, "orders-surla: CP3 reference host on http://%s/mcp\n", listener.Addr())
	fmt.Fprintf(output, "orders-surla: saving Change Cards in %s\n", cfg.cardsDir)
	fmt.Fprintln(output, "orders-surla: CP4 verification and CP5 promotion are not configured")
	finished := make(chan error, 1)
	go func() { finished <- srv.Serve(listener) }()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			err = errors.Join(err, srv.Close())
		}
		serveErr := <-finished
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}

// newHandler constructs the generated target client at the command's
// composition root and binds its updater to the existing Surla HTTP/MCP
// handler. The target token is never supplied by a tool argument, and the
// returned close function releases the cards directory lock.
func newHandler(cfg config) (http.Handler, func() error, error) {
	clientHTTP := &http.Client{CheckRedirect: rejectTargetRedirect}
	transport := genordersclient.NewClient(cfg.ordersScheme, cfg.ordersHost, clientHTTP,
		goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	client := genorders.NewClient(nil, nil, nil, transport.OverridePrompt(), nil, nil, nil)
	cp, err := controlplane.New(controlplane.Options{
		CardsDir:      cfg.cardsDir,
		TargetUpdater: &ordersPrompts{client: client, token: cfg.ordersToken},
	})
	if err != nil {
		return nil, nil, err
	}
	auth := controlplane.NewTokenAuthenticator(map[string]controlplane.Principal{cfg.authToken: {ID: cfg.principal}})
	return otelhttp.NewHandler(controlplane.NewHTTPHandler(cp, auth), "orders-surla"), cp.Close, nil
}

// rejectTargetRedirect keeps the operation and its bearer token at the
// operator's configured target. The generated API declares no redirect
// response, so its decoder reports the original 3xx as an operation error.
func rejectTargetRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// parseOrdersURL requires an origin because the generated client owns every
// route. Rejecting paths and URL credentials avoids silently discarding them.
func parseOrdersURL(raw string) (*url.URL, error) {
	origin, err := url.Parse(raw)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" ||
		origin.User != nil || (origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return nil, errors.New("-orders-url must be an HTTP(S) origin without a path, query, fragment, or URL credentials")
	}
	return origin, nil
}
