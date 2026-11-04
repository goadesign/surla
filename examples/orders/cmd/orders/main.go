// Command orders serves the orders API and runs the refund agent's workflows.
//
// It is the composition root: it reads flags and environment, constructs the
// dependency clients, the runtime store, the Temporal engine and the model
// client, and passes them to the service and agent packages. The runtime
// store is a SQLite database under --state-dir, so runs, checkpoints and
// records survive restarts. The --env flag
// only records which environment the commerce and payments endpoints belong
// to; the endpoints themselves come from --commerce-url and --payments-url.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	gencommerce "github.com/goadesign/surla/examples/orders/gen/commerce"
	gencommerceclient "github.com/goadesign/surla/examples/orders/gen/http/commerce/client"
	genordersserver "github.com/goadesign/surla/examples/orders/gen/http/orders/server"
	genorders "github.com/goadesign/surla/examples/orders/gen/orders"
	genrefund "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund"
	genrefundorders "github.com/goadesign/surla/examples/orders/gen/orders/agents/refund/orders"
	"github.com/goadesign/surla/examples/orders/internal/assistant"
	"github.com/goadesign/surla/examples/orders/internal/orders"
	"github.com/goadesign/surla/examples/orders/internal/refundflow"
	"github.com/goadesign/surla/examples/orders/internal/runstore"
	"github.com/goadesign/surla/examples/orders/internal/scriptedmodel"
	"go.temporal.io/sdk/client"
	"goa.design/goa-ai/features/model/anthropic"
	"goa.design/goa-ai/features/model/gateway"
	temporalengine "goa.design/goa-ai/runtime/agent/engine/temporal"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/prompt"
	"goa.design/goa-ai/runtime/agent/runtime"
	goahttp "goa.design/goa/v3/http"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// config holds the command's flags and environment.
type config struct {
	// env names the dependency environment: "sim" or "staging".
	env string
	// listen is the address the orders API listens on.
	listen string
	// commerceURL is the base URL of the commerce system.
	commerceURL string
	// temporalAddr is the Temporal frontend address.
	temporalAddr string
	// refundQueue is the task queue the refund worker polls.
	refundQueue string
	// stateDir is the directory holding the runtime store database.
	stateDir string
	// modelProvider selects the refund agent's model: "anthropic",
	// "gateway" (a goa-ai model gateway reached over gRPC), or "scripted",
	// the deterministic zero-key demo stand-in.
	modelProvider string
	// modelGateway is the gRPC address of the model gateway used with
	// --model-provider=gateway.
	modelGateway string
	// model is the Anthropic model the refund agent uses.
	model string
	// anthropicKey authenticates to Anthropic.
	anthropicKey string
	// signingKey verifies bearer tokens.
	signingKey []byte
}

const (
	// modelID is the runtime's registered name for the refund agent's model.
	modelID = "default"
	// runtimeDatabase is the runtime store's file name inside --state-dir.
	runtimeDatabase = "orders-runtime.db"
)

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		slog.Error("orders stopped", "error", err)
		os.Exit(1)
	}
}

// run wires the service and serves it until ctx ends. The runtime store is
// opened first and closed last, after the agent engine has stopped writing.
func run(ctx context.Context, cfg config) (err error) {
	commerce, err := newCommerceClient(cfg.commerceURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	store, err := runstore.Open(ctx, filepath.Join(cfg.stateDir, runtimeDatabase))
	if err != nil {
		return fmt.Errorf("open runtime store: %w", err)
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	temporal, err := client.Dial(client.Options{HostPort: cfg.temporalAddr})
	if err != nil {
		return fmt.Errorf("connect to temporal: %w", err)
	}
	defer temporal.Close()

	eng, err := temporalengine.NewWorker(temporalengine.Options{
		ClientOptions: &client.Options{HostPort: cfg.temporalAddr},
		WorkerOptions: temporalengine.WorkerOptions{TaskQueue: genrefund.DefaultTaskQueue},
	})
	if err != nil {
		return fmt.Errorf("create agent engine: %w", err)
	}
	defer func() { _ = eng.Close() }()

	// prompts holds prompt overrides set through override_prompt. It lives in
	// memory only, so restarting the service restores the built-in prompts.
	prompts := prompt.NewInMemoryStore()
	rt := runtime.New(store, runtime.WithEngine(eng), runtime.WithPromptStore(prompts))
	mc, err := newModelClient(cfg)
	if err != nil {
		return fmt.Errorf("create model client: %w", err)
	}
	if err := rt.RegisterModel(modelID, mc); err != nil {
		return fmt.Errorf("register model: %w", err)
	}

	conversations := assistant.NewConversations(rt, genrefund.NewClient(rt), store)
	svc := orders.New(commerce, refundflow.NewRunner(temporal, cfg.refundQueue), conversations,
		assistant.NewPrompts(rt.PromptRegistry), cfg.signingKey)
	endpoints := genorders.NewEndpoints(svc)

	if err := genrefund.RegisterRefundAgent(ctx, rt, genrefund.RefundAgentConfig{Planner: assistant.NewPlanner(modelID)}); err != nil {
		return fmt.Errorf("register refund agent: %w", err)
	}
	toolClient := genorders.NewClient(endpoints.Chat, endpoints.RunStatus, endpoints.ApproveRefund,
		endpoints.OverridePrompt, endpoints.ListOrders, endpoints.IssueStoreCredit, endpoints.RequestRefund)
	if err := genrefund.RegisterUsedToolsets(ctx, rt,
		genrefund.WithOrdersExecutor(genrefundorders.NewRefundOrdersExec(genrefundorders.WithClient(toolClient))),
	); err != nil {
		return fmt.Errorf("register refund tools: %w", err)
	}
	if err := rt.Seal(ctx); err != nil {
		return fmt.Errorf("seal runtime: %w", err)
	}

	mux := goahttp.NewMuxer()
	genordersserver.Mount(mux, genordersserver.New(endpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, errorHandler, nil))
	srv := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("orders listening", "addr", cfg.listen, "env", cfg.env, "model_provider", cfg.modelProvider)

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// parseConfig reads flags and the required secrets from the environment.
func parseConfig() (config, error) {
	var cfg config
	flag.StringVar(&cfg.env, "env", "", `dependency environment: "sim" or "staging"`)
	flag.StringVar(&cfg.listen, "listen", "localhost:8080", "address of the orders API")
	flag.StringVar(&cfg.commerceURL, "commerce-url", "", "base URL of the commerce system")
	flag.StringVar(&cfg.temporalAddr, "temporal", "localhost:7233", "Temporal frontend address")
	flag.StringVar(&cfg.refundQueue, "refund-queue", "orders.refunds", "task queue of the refund worker")
	flag.StringVar(&cfg.stateDir, "state-dir", ".surla/state", "directory of the durable runtime store ("+runtimeDatabase+")")
	flag.StringVar(&cfg.modelProvider, "model-provider", "anthropic", `refund agent model: "anthropic", "gateway", or "scripted" for the zero-key demo`)
	flag.StringVar(&cfg.modelGateway, "model-gateway", "", "gRPC address of the goa-ai model gateway (with --model-provider=gateway)")
	flag.StringVar(&cfg.model, "model", "claude-sonnet-5-5", "Anthropic model used by the refund agent")
	flag.Parse()
	if cfg.env != "sim" && cfg.env != "staging" {
		return config{}, errors.New(`--env must be "sim" or "staging"`)
	}
	if cfg.commerceURL == "" {
		return config{}, errors.New("--commerce-url is required")
	}
	switch cfg.modelProvider {
	case "anthropic":
		cfg.anthropicKey = os.Getenv("ANTHROPIC_API_KEY")
		if cfg.anthropicKey == "" {
			return config{}, errors.New("ANTHROPIC_API_KEY is required with --model-provider=anthropic")
		}
	case "gateway":
		if cfg.modelGateway == "" {
			return config{}, errors.New("--model-gateway is required with --model-provider=gateway")
		}
	case "scripted":
	default:
		return config{}, errors.New(`--model-provider must be "anthropic", "gateway" or "scripted"`)
	}
	cfg.signingKey = []byte(os.Getenv("ORDERS_JWT_KEY"))
	if len(cfg.signingKey) == 0 {
		return config{}, errors.New("ORDERS_JWT_KEY is required")
	}
	return cfg, nil
}

// newModelClient returns the refund agent's model client. The gateway client
// sends every model call to a goa-ai model gateway, which owns the provider.
// The scripted client is a deterministic demo stand-in, not a language model.
func newModelClient(cfg config) (model.Client, error) {
	switch cfg.modelProvider {
	case "gateway":
		conn, err := grpc.NewClient(cfg.modelGateway, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, fmt.Errorf("connect to model gateway %s: %w", cfg.modelGateway, err)
		}
		return gateway.NewGRPCClient(conn)
	case "scripted":
		return scriptedmodel.New()
	default:
		return anthropic.NewFromAPIKey(cfg.anthropicKey, cfg.model)
	}
}

// newCommerceClient returns the generated commerce client for baseURL.
func newCommerceClient(baseURL string) (*gencommerce.Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse --commerce-url: %w", err)
	}
	c := gencommerceclient.NewClient(u.Scheme, u.Host, http.DefaultClient, goahttp.RequestEncoder, goahttp.ResponseDecoder, false)
	return gencommerce.NewClient(c.ListOrders(), c.GetOrder()), nil
}

// errorHandler logs errors the HTTP layer could not encode.
func errorHandler(ctx context.Context, _ http.ResponseWriter, err error) {
	slog.ErrorContext(ctx, "encode response", "error", err)
}
