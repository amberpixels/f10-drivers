package notion

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// DefaultMCPURL is Notion's hosted MCP server.
const DefaultMCPURL = "https://mcp.notion.com/mcp"

// MCPProtocolVersion is the MCP revision the client speaks.
const MCPProtocolVersion = "2025-06-18"

// mcpProtocols are the MCP revisions the client can speak, the one it asks
// for first; the requests it sends are the same in each.
var mcpProtocols = []string{MCPProtocolVersion, "2025-03-26"}

// errSessionGone is a 404 to a request carrying a session id: the server
// ended the session, and a new initialize starts another.
var errSessionGone = errors.New("notion mcp: the session ended")

// ErrNoLogin is a verb run without a stored login, or with one Notion no
// longer accepts.
var ErrNoLogin = errors.New("no Notion login: run f10-drivers notion login")

// tokens hands the MCP client its bearer token, and a fresh one after a 401.
type tokens interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context) (string, error)
}

// mcpClient calls tools on one MCP server over streamable HTTP: the
// handshake once per process, then tools/call.
type mcpClient struct {
	url     string
	tokens  tokens
	version string // the client version sent in initialize
	read    *http.Client
	write   *http.Client

	mu       sync.Mutex // one call at a time: the id, the session and the token refresh are shared
	session  string
	protocol string // the version initialize settled on
	ready    bool
	id       int
}

func newMCPClient(url string, tokens tokens, version string) *mcpClient {
	return &mcpClient{
		url:     url,
		tokens:  tokens,
		version: version,
		read:    &http.Client{Timeout: readTimeout},
		write:   &http.Client{Timeout: writeTimeout},
	}
}

// ToolError is a tool's answer that it failed, in the tool's words.
type ToolError struct {
	Tool string
	Text string
}

func (e *ToolError) Error() string { return fmt.Sprintf("notion mcp: %s: %s", e.Tool, e.Text) }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

var unknownTool = regexp.MustCompile(`(?i)unknown tool|no such tool|tool .*not found`)

// shortError keeps a tool error to one line when Notion appends its
// structured form: the code, the message, and when to retry. Other text is
// kept as it is.
func shortError(text string) string {
	i := strings.LastIndex(text, "\n\n{")
	if i < 0 {
		return text
	}

	var e struct {
		Error struct {
			Code         string `json:"code"`
			Message      string `json:"message"`
			Impact       string `json:"impact"`
			Retryability string `json:"retryability"`
		} `json:"error"`
	}

	if json.Unmarshal([]byte(text[i+2:]), &e) != nil || e.Error.Message == "" {
		return text
	}

	short := strings.TrimSpace(e.Error.Code + ": " + e.Error.Message + " " + e.Error.Impact)
	if e.Error.Retryability == "later" {
		short += " Try again later."
	}

	return short
}

var notFoundText = regexp.MustCompile(`(?i)could not find|not found|object_not_found`)

// call runs a tool and decodes its text answer, JSON, into out. A write is
// sent once (a 429 aside): an answer that never arrives is an
// *UnknownOutcomeError.
func (c *mcpClient) call(ctx context.Context, tool string, args any, write bool, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.handshake(ctx); err != nil {
		return err
	}

	params := map[string]any{"name": tool, "arguments": args}

	raw, err := c.rpc(ctx, write, "tools/call", params)
	if errors.Is(err, errSessionGone) {
		// The server ended the session before reading the call, so even a
		// write is safe to send again.
		c.ready, c.session = false, ""

		if err := c.handshake(ctx); err != nil {
			return err
		}

		raw, err = c.rpc(ctx, write, "tools/call", params)
	}

	if rerr, ok := errors.AsType[*rpcError](err); ok {
		if unknownTool.MatchString(rerr.Message) {
			return fmt.Errorf("notion mcp: the server has no tool %q any more; Notion changed its MCP tools (%s)",
				tool, rerr.Message)
		}

		return fmt.Errorf("notion mcp: %s: %s", tool, rerr.Message)
	}

	if err != nil {
		return err
	}

	var res toolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return c.unreadable(write, tool, err)
	}

	var text strings.Builder
	for _, part := range res.Content {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}

	if res.IsError {
		if notFoundText.MatchString(text.String()) {
			return &APIError{Status: http.StatusNotFound, Code: "object_not_found", Message: text.String()}
		}

		return &ToolError{Tool: tool, Text: shortError(text.String())}
	}

	if err := json.Unmarshal([]byte(text.String()), out); err != nil {
		return c.unreadable(write, tool, err)
	}

	return nil
}

func (c *mcpClient) unreadable(write bool, tool string, err error) error {
	if write {
		return fmt.Errorf("notion mcp: %s succeeded, but its answer could not be read: %w", tool, err)
	}

	return fmt.Errorf("notion mcp: %s answered in a shape the driver does not know: %w", tool, err)
}

func (c *mcpClient) handshake(ctx context.Context) error {
	if c.ready {
		return nil
	}

	raw, err := c.rpc(ctx, false, "initialize", map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "f10-drivers", "version": c.version},
	})
	if err != nil {
		return err
	}

	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}

	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("notion mcp: initialize: %w", err)
	}

	if !slices.Contains(mcpProtocols, res.ProtocolVersion) {
		return fmt.Errorf("notion mcp: the server speaks MCP %q; f10-drivers speaks %s",
			res.ProtocolVersion, strings.Join(mcpProtocols, ", "))
	}

	c.protocol, c.ready = res.ProtocolVersion, true

	return c.notify(ctx, "notifications/initialized")
}

// rpc sends one request and returns its result. Reads retry a 429 or a
// transient 5xx; a write retries only a 429. A 401 refreshes the token once.
func (c *mcpClient) rpc(ctx context.Context, write bool, method string, params any) (json.RawMessage, error) {
	c.id++
	id := c.id

	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}

	data, err := c.post(ctx, write, payload, true)
	if err != nil {
		return nil, err
	}

	resp, err := decodeResponse(data, id)
	if err != nil {
		if write {
			return nil, fmt.Errorf("notion mcp: %s succeeded, but its answer could not be read: %w", method, err)
		}

		return nil, fmt.Errorf("notion mcp: %s: %w", method, err)
	}

	if resp.Error != nil {
		return nil, resp.Error
	}

	return resp.Result, nil
}

func (e *rpcError) Error() string { return fmt.Sprintf("notion mcp: %s (%d)", e.Message, e.Code) }

func (c *mcpClient) notify(ctx context.Context, method string) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}

	_, err = c.post(ctx, false, payload, false)

	return err
}

type httpBody struct {
	contentType string
	data        []byte
}

func (c *mcpClient) post(ctx context.Context, write bool, payload []byte, wantBody bool) (httpBody, error) {
	hc := c.read
	if write {
		hc = c.write
	}

	refreshed := false

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return httpBody{}, err // nothing was sent
		}

		token, err := c.tokens.Token(ctx)
		if err != nil {
			return httpBody{}, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
		if err != nil {
			return httpBody{}, err
		}

		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		if c.session != "" {
			req.Header.Set("Mcp-Session-Id", c.session)
		}

		if c.ready {
			req.Header.Set("MCP-Protocol-Version", c.protocol)
		}

		resp, err := hc.Do(req)
		if err != nil {
			if write {
				return httpBody{}, &UnknownOutcomeError{Err: fmt.Errorf("notion mcp: %w", err)}
			}

			return httpBody{}, fmt.Errorf("notion mcp: %w", err)
		}

		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			c.session = s
		}

		if resp.StatusCode == http.StatusNotFound && c.session != "" {
			return httpBody{}, errSessionGone
		}

		if resp.StatusCode == http.StatusUnauthorized {
			if refreshed {
				return httpBody{}, ErrNoLogin
			}

			if _, err := c.tokens.Refresh(ctx); err != nil {
				return httpBody{}, err
			}

			refreshed = true

			continue
		}

		if retryable(write, resp.StatusCode) && attempt < maxRetries {
			if err := sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return httpBody{}, err
			}

			continue
		}

		if resp.StatusCode/100 != 2 {
			err := fmt.Errorf("notion mcp: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
			if write && resp.StatusCode/100 == 5 {
				return httpBody{}, &UnknownOutcomeError{Err: err}
			}

			return httpBody{}, err
		}

		if readErr != nil && write {
			return httpBody{}, fmt.Errorf(
				"notion mcp: the call succeeded, but its answer could not be read: %w",
				readErr,
			)
		}

		if readErr != nil {
			return httpBody{}, fmt.Errorf("notion mcp: %w", readErr)
		}

		if !wantBody {
			return httpBody{}, nil
		}

		return httpBody{contentType: resp.Header.Get("Content-Type"), data: data}, nil
	}
}

// decodeResponse reads the JSON-RPC response with the given id from a JSON
// body or an event stream.
func decodeResponse(b httpBody, id int) (*rpcResponse, error) {
	mt, _, _ := mime.ParseMediaType(b.contentType)

	switch mt {
	case "application/json":
		var resp rpcResponse
		if err := json.Unmarshal(b.data, &resp); err != nil {
			return nil, err
		}

		return &resp, nil
	case "text/event-stream":
		return fromEvents(b.data, id)
	default:
		return nil, fmt.Errorf("unexpected content type %q", b.contentType)
	}
}

// fromEvents finds the response among server-sent events, skipping any
// notification the server sends before it.
func fromEvents(data []byte, id int) (*rpcResponse, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)

	var event strings.Builder

	flush := func() (*rpcResponse, bool) {
		defer event.Reset()

		var resp rpcResponse
		if event.Len() == 0 || json.Unmarshal([]byte(event.String()), &resp) != nil {
			return nil, false
		}

		return &resp, resp.ID != nil && *resp.ID == id
	}

	for sc.Scan() {
		line := sc.Text()

		switch {
		case line == "":
			if resp, ok := flush(); ok {
				return resp, nil
			}
		case strings.HasPrefix(line, "data:"):
			if event.Len() > 0 {
				event.WriteByte('\n')
			}

			event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}

	if resp, ok := flush(); ok {
		return resp, nil
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	return nil, errors.New("the event stream ended without a response")
}
