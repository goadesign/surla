package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeOwnsItsListenerUntilCancellation starts the actual command server,
// reads its authenticated checkpoint resource, checks that a second host
// cannot share the same cards directory while running, and waits for shutdown.
// The target is never called by this resource request.
func TestServeOwnsItsListenerUntilCancellation(t *testing.T) {
	cardsDir := t.TempDir()
	cfg, err := parseConfig([]string{"-listen", "127.0.0.1:0", "-principal", "operator",
		"-auth-token", "mcp", "-orders-token", "target", "-orders-url", "http://127.0.0.1:1",
		"-cards-dir", cardsDir},
		func(string) string { return "" }, io.Discard)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	finished := make(chan error, 1)
	go func() {
		finished <- serve(ctx, cfg, writer)
		writer.Close()
	}()
	scanner := bufio.NewScanner(reader)
	require.True(t, scanner.Scan(), "server did not report its listening address")
	endpoint := strings.TrimPrefix(scanner.Text(), "orders-surla: CP3 reference host on ")
	require.True(t, scanner.Scan(), "server did not report its cards directory")
	assert.Equal(t, "orders-surla: saving Change Cards in "+cardsDir, scanner.Text())
	require.True(t, scanner.Scan(), "server did not report its limited configuration")
	assert.Equal(t, "orders-surla: CP4 verification and CP5 promotion are not configured", scanner.Text())
	require.ErrorContains(t, serve(ctx, cfg, io.Discard), "is in use by another process")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/mcp")+"/ui/checkpoint", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer mcp")
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	cancel()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
	_, closeCP, err := newHandler(cfg)
	require.NoError(t, err, "shutdown must release the cards directory lock")
	require.NoError(t, closeCP())
}

// TestConfigKeepsTheTwoCredentialsSeparate checks environment resolution,
// explicit flag precedence, and help output without environment secrets.
func TestConfigKeepsTheTwoCredentialsSeparate(t *testing.T) {
	env := map[string]string{"SURLA_AUTH_TOKEN": "mcp-secret", "ORDERS_PROMPTS_TOKEN": "target-secret"}
	getenv := func(key string) string { return env[key] }
	args := []string{"-principal", "operator", "-orders-url", "http://127.0.0.1:8080"}
	cfg, err := parseConfig(args, getenv, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "mcp-secret", cfg.authToken)
	assert.Equal(t, "target-secret", cfg.ordersToken)
	assert.Equal(t, ".surla/orders-surla-cards", cfg.cardsDir)
	assert.Equal(t, "http", cfg.ordersScheme)
	assert.Equal(t, "127.0.0.1:8080", cfg.ordersHost)
	cfg, err = parseConfig(append(args, "-auth-token", "explicit-mcp", "-orders-token", "explicit-target", "-cards-dir", "/tmp/custom-cards"), getenv, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "explicit-mcp", cfg.authToken)
	assert.Equal(t, "explicit-target", cfg.ordersToken)
	assert.Equal(t, "/tmp/custom-cards", cfg.cardsDir)
	var help bytes.Buffer
	_, err = parseConfig([]string{"-h"}, getenv, &help)
	require.True(t, errors.Is(err, flag.ErrHelp))
	assert.Contains(t, help.String(), "SURLA_AUTH_TOKEN")
	assert.Contains(t, help.String(), "ORDERS_PROMPTS_TOKEN")
	assert.Contains(t, help.String(), ".surla/orders-surla-cards")
	assert.NotContains(t, help.String(), "mcp-secret")
	assert.NotContains(t, help.String(), "target-secret")
}

// TestConfigRejectsOriginsTheGeneratedClientCannotUse ensures that the
// command never silently discards a path or URL credential.
func TestConfigRejectsOriginsTheGeneratedClientCannotUse(t *testing.T) {
	for _, origin := range []string{"", "ftp://example.invalid", "http://:8080", "http://example.invalid/api", "http://u:p@example.invalid", "http://example.invalid?x=1", "http://example.invalid#part"} {
		t.Run(origin, func(t *testing.T) {
			_, err := parseConfig([]string{"-principal", "operator", "-auth-token", "mcp", "-orders-token", "target", "-orders-url", origin}, func(string) string { return "" }, io.Discard)
			require.ErrorContains(t, err, "-orders-url must be")
		})
	}
	_, err := parseConfig([]string{"-principal", "operator", "-orders-url", "http://127.0.0.1:8080"}, func(string) string { return "" }, io.Discard)
	require.ErrorContains(t, err, "bearer token")
	_, err = parseConfig([]string{"-principal", "operator\xff", "-auth-token", "mcp", "-orders-token", "target", "-orders-url", "http://127.0.0.1:8080"}, func(string) string { return "" }, io.Discard)
	require.ErrorContains(t, err, "-principal must be valid UTF-8")
}
