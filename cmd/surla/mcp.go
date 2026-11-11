// This file is the composition root of `surla mcp`, the local stdio MCP
// server that coding agents launch as a child process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	genmcpsurla "github.com/goadesign/surla/gen/jsonrpc/mcp_surla/server"
	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"go.opentelemetry.io/otel"
)

// runMCP starts the Surla MCP JSON-RPC server over stdin and stdout, binding
// every request on the stdio session to the operator principal (-principal or
// uid:<euid>) and writing startup status and telemetry to stderr.
func runMCP(args []string, stdin, stdout *os.File, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var principalFlag string
	var principalSet bool
	fs.StringVar(&principalFlag, "principal", "", "Operator principal ID that owns cards, runs, and traces in this session (default: uid:<euid>)")
	var cfg serveConfig
	bindServeConfigFlags(fs, &cfg)
	if err := parseServeConfigFlags(fs, args, &cfg); err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "principal" {
			principalSet = true
		}
	})
	principal := fmt.Sprintf("uid:%d", os.Geteuid())
	if principalSet {
		if !utf8.ValidString(principalFlag) {
			return errors.New("-principal must be valid UTF-8")
		}
		principal = strings.TrimSpace(principalFlag)
		if principal == "" {
			return errors.New("-principal must not be blank")
		}
	}
	tracerProvider, err := newTraceProvider(context.Background(), stderr, os.Getenv)
	if err != nil {
		return err
	}
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer func() {
		// The later control-plane defer finishes owned work and closes its file
		// first. This also flushes startup evidence when construction fails.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = errors.Join(err, tracerProvider.Shutdown(flushCtx))
		otel.SetTracerProvider(previousProvider)
	}()
	opts, err := serveOptions(cfg, os.Getenv, os.Environ())
	if err != nil {
		return err
	}
	opts.TracerProvider = tracerProvider
	cp, err := controlplane.New(opts)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cp.Close()) }()
	starterProject := cfg.projectDir
	if cfg.verificationPath != "" {
		starterProject = ""
	}
	writeMCPStatus(stderr, principal, opts, starterProject)
	endpoints := gensurla.NewEndpoints(cp)
	mcpAdapter := genmcpadapter.NewMCPAdapter(endpoints, nil)
	mcpEndpoints := genmcpadapter.NewEndpoints(mcpAdapter)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = controlplane.WithPrincipal(ctx, controlplane.Principal{ID: principal})
	serveErr := genmcpsurla.ServeStdio(ctx, stdin, stdout, mcpEndpoints)
	if serveErr != nil && ctx.Err() != nil && errors.Is(serveErr, context.Canceled) {
		return nil
	}
	return serveErr
}

// writeMCPStatus tells the operator or MCP host log on w (stderr) that `surla
// mcp` is serving MCP over stdio, which principal owns the session's records,
// where Change Cards are saved, and which of the five checkpoints (CP1–CP5)
// are on or off.
func writeMCPStatus(w io.Writer, principal string, opts controlplane.Options, project string) {
	fmt.Fprintf(w, "surla: serving MCP over stdio as principal %s (architecture guide: surla://golden-path)\n", principal)
	writeCheckpointStatus(w, opts, project)
}
