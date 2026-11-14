// This file binds the team reminder sender to the local Chorus server's MCP endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/goadesign/surla/internal/project"
)

type (
	// teamChorusSender posts one reminder as two Chorus tool calls: who registers
	// Surla's handle in the team, then send hands the fixed text to one role.
	// Its client is its own, bounded per post; it never follows redirects.
	teamChorusSender struct {
		client    *http.Client
		chorusURL string
	}
	// teamWhoArguments are exactly the arguments of Chorus's who tool that Surla sends.
	teamWhoArguments struct {
		Agent string `json:"agent"`
		Swarm string `json:"swarm"`
		Model string `json:"model"`
		Role  string `json:"role"`
	}
	// teamSendArguments are exactly the arguments of Chorus's send tool that
	// Surla sends: no channel, reply, pin or wait.
	teamSendArguments struct {
		Agent string `json:"agent"`
		To    string `json:"to"`
		Text  string `json:"text"`
	}
	// teamToolRequest is one JSON-RPC tools/call request, as Chorus's own
	// dashboard writes it: no earlier handshake, the protocol version in _meta.
	teamToolRequest struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      int            `json:"id"`
		Method  string         `json:"method"`
		Params  teamToolParams `json:"params"`
	}
	// teamToolParams names the tool and carries its typed arguments.
	teamToolParams struct {
		Meta      teamToolMeta `json:"_meta"`
		Name      string       `json:"name"`
		Arguments any          `json:"arguments"`
	}
	// teamToolMeta is the per-request protocol metadata Chorus requires.
	teamToolMeta struct {
		ProtocolVersion    string         `json:"io.modelcontextprotocol/protocolVersion"`
		ClientCapabilities struct{}       `json:"io.modelcontextprotocol/clientCapabilities"`
		ClientInfo         teamClientInfo `json:"io.modelcontextprotocol/clientInfo"`
	}
	// teamClientInfo identifies Surla as the calling client.
	teamClientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	// teamToolResponse decodes only whether the call failed. The result's content,
	// including any unread messages Chorus piggybacks on send, has no field here,
	// and the error's object is decoded into an empty struct, so no response
	// text is kept.
	teamToolResponse struct {
		Error  *struct{} `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
)

const (
	// teamChorusURL is the local Chorus server that both the view reader and the
	// reminder sender call.
	teamChorusURL = "http://127.0.0.1:9440"
	// teamPostTimeout bounds each reminder post, connection to last byte. Two roles
	// with two posts each hold the supervisor at most 20 seconds, under its
	// 45-second stream stall bound.
	teamPostTimeout = 5 * time.Second
	// teamPostResponseMaxBytes bounds the bytes read from each post's response.
	// Chorus returns at most 20 unread messages of at most 4000 characters with a
	// send, about 0.5 MiB even when every character is escaped.
	teamPostResponseMaxBytes = 2 << 20
	// teamMCPProtocol is the MCP protocol version Chorus's dashboard writes.
	// A Chorus that stops accepting it makes every reminder fail with error status.
	teamMCPProtocol = "2026-07-28"
	// teamSurlaModel is the model Surla declares when it registers; it runs none.
	teamSurlaModel = "surla"
	// teamSurlaRole is the role text the team's Chorus members see for Surla.
	teamSurlaRole = "Surla server. Sends reminders only. Reads no replies."
)

// Send registers Surla's handle in the team, then sends the reminder as a direct
// message to the role. A failed who sends nothing more. It returns nil or a
// project.TeamSendFailure, never response text.
func (s teamChorusSender) Send(ctx context.Context, reminder project.TeamReminder) error {
	who := teamWhoArguments{Agent: reminder.From, Swarm: reminder.Team, Model: teamSurlaModel, Role: teamSurlaRole}
	if err := s.post(ctx, "who", who); err != nil {
		return err
	}
	return s.post(ctx, "send", teamSendArguments{Agent: reminder.From, To: "@" + reminder.To, Text: reminder.Text})
}

// newTeamChorusSender returns the production sender with its own bounded client.
func newTeamChorusSender() teamChorusSender {
	return teamChorusSender{
		client: &http.Client{
			Timeout:       teamPostTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		chorusURL: teamChorusURL,
	}
}

// post makes one tools/call POST to /mcp and classifies its failure. Success
// is HTTP 200 with a decodable JSON-RPC result that is not marked isError.
func (s teamChorusSender) post(ctx context.Context, tool string, arguments any) error {
	fail := func(kind string) error { return project.TeamSendFailure{Post: tool, Kind: kind} }
	body, err := json.Marshal(teamToolRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params: teamToolParams{
			Meta: teamToolMeta{
				ProtocolVersion: teamMCPProtocol,
				ClientInfo:      teamClientInfo{Name: "surla", Version: "1"},
			},
			Name:      tool,
			Arguments: arguments,
		},
	})
	if err != nil {
		return fail("connection")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.chorusURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return fail("connection")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("MCP-Protocol-Version", teamMCPProtocol)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", tool)
	response, err := s.client.Do(request)
	if err != nil {
		return fail(teamPostErrorKind(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fail("http_status")
	}
	kind := readTeamToolResponse(response.Body)
	if kind != "" {
		return fail(kind)
	}
	return nil
}

// readTeamToolResponse reads at most one byte past teamPostResponseMaxBytes and
// returns "" for a successful tool result, or the failure kind. A body longer
// than the bound is a response failure, even when the bytes up to the bound
// would decode on their own.
func readTeamToolResponse(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, teamPostResponseMaxBytes+1))
	if err != nil {
		return teamPostErrorKind(err)
	}
	if len(raw) > teamPostResponseMaxBytes {
		return "response"
	}
	var decoded teamToolResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "response"
	}
	switch {
	case decoded.Error != nil:
		return "rpc_error"
	case decoded.Result == nil:
		return "response"
	case decoded.Result.IsError:
		return "tool_error"
	}
	return ""
}

// teamPostErrorKind tells a post that ran out of time from a refused or broken
// connection.
func teamPostErrorKind(err error) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	return "connection"
}
