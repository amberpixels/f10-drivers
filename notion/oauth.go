package notion

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// loginTimeout bounds the wait for the browser to come back with a code.
const loginTimeout = 5 * time.Minute

// refreshMargin refreshes an access token this long before it expires.
const refreshMargin = time.Minute

// storedLogin is one user's OAuth login to one MCP server, stored outside the
// project (tokens.go).
type storedLogin struct {
	Server    string    `json:"server"`
	Resource  string    `json:"resource"`
	ClientID  string    `json:"client_id"`
	TokenURL  string    `json:"token_url"`
	RevokeURL string    `json:"revoke_url,omitempty"`
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh,omitempty"`
	Expiry    time.Time `json:"expiry,omitzero"`
}

// authServer is what discovery finds: the protected resource and the
// endpoints of the server that issues its tokens.
type authServer struct {
	Resource     string
	Scope        string
	Issuer       string
	Authorize    string
	Token        string
	Registration string
	Revocation   string
	ISSParam     bool // the server sends iss with the code (RFC 9207)
}

// LoginOptions configures Login: the server, how to open a url in the
// browser, and where to print what the user should do.
type LoginOptions struct {
	Server string
	Open   func(url string) error
	Out    io.Writer
}

// Login runs OAuth 2.1 against the server's advertised authorization
// server: dynamic registration as a public client, then an authorization
// code with PKCE delivered to a loopback listener. It stores the tokens.
func Login(ctx context.Context, opt LoginOptions) error {
	hc := &http.Client{Timeout: readTimeout}

	as, err := discover(ctx, hc, opt.Server)
	if err != nil {
		return err
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	redirect := fmt.Sprintf("http://%s/callback", ln.Addr())

	clientID, err := register(ctx, hc, as.Registration, redirect)
	if err != nil {
		return err
	}

	verifier, state := random(), random()
	sum := sha256.Sum256([]byte(verifier))

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {as.Resource},
	}
	if as.Scope != "" {
		q.Set("scope", as.Scope)
	}

	authURL := as.Authorize + "?" + q.Encode()

	codes := make(chan callback, 1)
	srv := &http.Server{Handler: callbackHandler(codes, state), ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	_, _ = fmt.Fprintf(opt.Out, "Log in to Notion in the browser; if it does not open, open:\n%s\n", authURL)
	if opt.Open != nil {
		_ = opt.Open(authURL) // the printed url is the fallback
	}

	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var cb callback

	select {
	case <-ctx.Done():
		return fmt.Errorf("login: no answer from the browser: %w", ctx.Err())
	case cb = <-codes:
	}

	switch {
	case cb.err != "":
		return fmt.Errorf("login: Notion answered %s", cb.err)
	case cb.state != state:
		return errors.New("login: the answer's state does not match this login")
	case cb.iss != "" && cb.iss != as.Issuer, cb.iss == "" && as.ISSParam:
		return fmt.Errorf("login: the answer comes from %q, not %q", cb.iss, as.Issuer)
	}

	l := &storedLogin{
		Server: opt.Server, Resource: as.Resource, ClientID: clientID, TokenURL: as.Token, RevokeURL: as.Revocation,
	}

	err = l.grant(ctx, hc, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	})
	if err != nil {
		return err
	}

	where, err := defaultStore().save(l)
	if err != nil {
		return fmt.Errorf("login: storing the token: %w", err)
	}

	_, _ = fmt.Fprintf(opt.Out, "Logged in to %s; the token is in %s.\n", opt.Server, where)

	return nil
}

// Logout revokes the stored refresh token, best effort, and forgets the login.
func Logout(ctx context.Context, server string, out io.Writer) error {
	store := defaultStore()

	l, err := store.load(server)
	if errors.Is(err, errNotStored) {
		_, _ = fmt.Fprintf(out, "Not logged in to %s.\n", server)

		return nil
	}

	if err != nil {
		return err
	}

	if l.RevokeURL != "" && l.Refresh != "" {
		form := url.Values{"token": {l.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {l.ClientID}}
		if resp, err := postForm(ctx, &http.Client{Timeout: readTimeout}, l.RevokeURL, form); err == nil {
			_ = resp.Body.Close()
		}
	}

	if err := store.remove(server); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "Logged out of %s.\n", server)

	return nil
}

type callback struct{ code, state, iss, err string }

// callbackHandler takes the browser's answer to Login and shows the user
// how it went; Login decides, the page only reflects the state and error.
func callbackHandler(codes chan<- callback, state string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)

			return
		}

		q := r.URL.Query()
		cb := callback{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss"), err: q.Get("error")}

		if d := q.Get("error_description"); d != "" {
			cb.err += ": " + d
		}

		select {
		case codes <- cb:
		default:
			callbackPage(
				w,
				http.StatusConflict,
				"⚠️",
				"This login already has an answer",
				"Run the login again if it failed.",
			)

			return
		}

		switch {
		case cb.err != "":
			callbackPage(w, http.StatusOK, "❌", "Notion did not log you in", cb.err+". The terminal has the details.")
		case cb.state != state:
			callbackPage(
				w,
				http.StatusOK,
				"❌",
				"This answer belongs to another login",
				"Run f10-drivers notion login again.",
			)
		default:
			callbackPage(w, http.StatusOK, "✅", "Logged in to Notion",
				"f10-drivers can reach your workspace now. You can close this tab.")
		}
	})
}

var callbackTemplate = template.Must(template.New("callback").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>f10-drivers</title>
<style>
:root { color-scheme: light dark; --bg: #f6f6f4; --card: #fff; --fg: #1f1f1f; --muted: #6b6b6b; --line: #e3e3e0; }
@media (prefers-color-scheme: dark) { :root { --bg: #161616; --card: #202020; --fg: #ececec; --muted: #9a9a9a; --line: #333; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg);
  font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, sans-serif; }
.card { max-width: 26rem; margin: 1rem; padding: 2.5rem 2rem; text-align: center; background: var(--card);
  border: 1px solid var(--line); border-radius: 14px; box-shadow: 0 8px 30px rgb(0 0 0 / 8%); }
.icon { font-size: 3rem; line-height: 1; }
h1 { margin: 1rem 0 .5rem; font-size: 1.35rem; }
p { margin: 0; color: var(--muted); }
</style></head>
<body><div class="card"><div class="icon">{{.Icon}}</div><h1>{{.Title}}</h1><p>{{.Text}}</p></div></body></html>
`))

// callbackPage shows the user how the login went, in the tab Notion
// redirected back to.
func callbackPage(w http.ResponseWriter, status int, icon, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = callbackTemplate.Execute(w, struct{ Icon, Title, Text string }{icon, title, text})
}

// Session is a stored login in use: it hands out the access token and
// refreshes it, writing the new tokens back.
type Session struct {
	login *storedLogin
	store tokenStore
	hc    *http.Client
}

// LoadSession reads the stored login for server, or fails with ErrNoLogin.
func LoadSession(_ context.Context, server string) (*Session, error) {
	store := defaultStore()

	l, err := store.load(server)
	if errors.Is(err, errNotStored) {
		return nil, ErrNoLogin
	}

	if err != nil {
		return nil, err
	}

	return &Session{login: l, store: store, hc: &http.Client{Timeout: readTimeout}}, nil
}

// Token returns the access token, refreshed first when it is about to expire.
func (s *Session) Token(ctx context.Context) (string, error) {
	if !s.login.Expiry.IsZero() && time.Until(s.login.Expiry) < refreshMargin && s.login.Refresh != "" {
		return s.Refresh(ctx)
	}

	return s.login.Access, nil
}

// Refresh trades the refresh token for new tokens. A refresh Notion refuses
// is a login to redo.
func (s *Session) Refresh(ctx context.Context) (string, error) {
	if s.login.Refresh == "" {
		return "", ErrNoLogin
	}

	err := s.login.grant(ctx, s.hc, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.login.Refresh},
	})

	var oerr *oauthError
	if errors.As(err, &oerr) && oerr.status/100 == 4 {
		// Another run may have refreshed first, spending this refresh token.
		if l, lerr := s.store.load(s.login.Server); lerr == nil && l.Refresh != s.login.Refresh && l.Access != "" {
			s.login = l

			return l.Access, nil
		}

		return "", fmt.Errorf("the Notion login expired or was revoked (%s): run f10-drivers notion login", oerr.Code)
	}

	if err != nil {
		return "", err
	}

	if _, err := s.store.save(s.login); err != nil {
		return "", fmt.Errorf("storing the refreshed token: %w", err)
	}

	return s.login.Access, nil
}

type oauthError struct {
	status      int
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oauth: %s: %s", e.Code, e.Description)
	}

	return fmt.Sprintf("oauth: %s (HTTP %d)", e.Code, e.status)
}

// grant posts a token request and keeps the tokens it answers; a refresh
// that brings no new refresh token keeps the old one.
func (l *storedLogin) grant(ctx context.Context, hc *http.Client, form url.Values) error {
	form.Set("client_id", l.ClientID)
	form.Set("resource", l.Resource)

	resp, err := postForm(ctx, hc, l.TokenURL, form)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		oerr := &oauthError{status: resp.StatusCode}
		_ = json.Unmarshal(data, oerr)

		return oerr
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("oauth: the token endpoint answered no access token: %s", data)
	}

	l.Access = tok.AccessToken
	if tok.RefreshToken != "" {
		l.Refresh = tok.RefreshToken
	}

	l.Expiry = time.Time{}
	if tok.ExpiresIn > 0 {
		l.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}

	return nil
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// discover follows the MCP authorization spec: the 401's resource_metadata
// (else the well-known path), then the authorization server's metadata.
func discover(ctx context.Context, hc *http.Client, server string) (*authServer, error) {
	srv, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	probe, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": map[string]any{
		"protocolVersion": MCPProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "f10-drivers", "version": clientVersion()},
	}})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(probe))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login: reaching %s: %w", server, err)
	}

	_ = resp.Body.Close()

	params := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(resp.Header.Get("WWW-Authenticate"), -1) {
		params[m[1]] = m[2]
	}

	metaURL := params["resource_metadata"]
	if metaURL == "" {
		metaURL = srv.Scheme + "://" + srv.Host + "/.well-known/oauth-protected-resource" + srv.Path
	}

	if !secure(metaURL) {
		return nil, fmt.Errorf("login: refusing %q: the resource metadata must be https", metaURL)
	}

	var res struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}

	if err := getJSON(ctx, hc, metaURL, &res); err != nil {
		return nil, err
	}

	if len(res.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("login: %s names no authorization server", metaURL)
	}

	// RFC 9728: a token asked for another resource would go to that resource.
	if res.Resource != "" && strings.TrimSuffix(res.Resource, "/") != strings.TrimSuffix(server, "/") {
		return nil, fmt.Errorf("login: %s describes %q, not %q", metaURL, res.Resource, server)
	}

	issuer, err := url.Parse(res.AuthorizationServers[0])
	if err != nil {
		return nil, err
	}

	var meta struct {
		Issuer       string   `json:"issuer"`
		Authorize    string   `json:"authorization_endpoint"`
		Token        string   `json:"token_endpoint"`
		Registration string   `json:"registration_endpoint"`
		Revocation   string   `json:"revocation_endpoint"`
		Challenges   []string `json:"code_challenge_methods_supported"`
		ISSParam     bool     `json:"authorization_response_iss_parameter_supported"`
	}

	asURL := issuer.Scheme + "://" + issuer.Host + "/.well-known/oauth-authorization-server" + strings.TrimSuffix(
		issuer.Path,
		"/",
	)
	if err := getJSON(ctx, hc, asURL, &meta); err != nil {
		return nil, err
	}

	switch {
	case strings.TrimSuffix(meta.Issuer, "/") != strings.TrimSuffix(res.AuthorizationServers[0], "/"):
		return nil, fmt.Errorf("login: %s names issuer %q, not %q", asURL, meta.Issuer, res.AuthorizationServers[0])
	case meta.Registration == "":
		return nil, fmt.Errorf("login: %s offers no client registration", meta.Issuer)
	case len(meta.Challenges) > 0 && !slices.Contains(meta.Challenges, "S256"):
		return nil, fmt.Errorf("login: %s does not take PKCE S256", meta.Issuer)
	}

	for _, u := range []string{meta.Authorize, meta.Token, meta.Registration, meta.Revocation} {
		if u != "" && !secure(u) {
			return nil, fmt.Errorf("login: refusing %q: an OAuth endpoint must be https", u)
		}
	}

	as := &authServer{
		Resource: cmp.Or(res.Resource, server), Scope: params["scope"], Issuer: meta.Issuer,
		Authorize: meta.Authorize, Token: meta.Token, Registration: meta.Registration,
		Revocation: meta.Revocation, ISSParam: meta.ISSParam,
	}
	if as.Scope == "" {
		as.Scope = strings.Join(res.ScopesSupported, " ")
	}

	return as, nil
}

// register registers f10-drivers as a public client for this login's
// loopback redirect (RFC 7591).
func register(ctx context.Context, hc *http.Client, endpoint, redirect string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "f10-drivers",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("login: registering the client: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	var reg struct {
		ClientID string `json:"client_id"`
	}

	if resp.StatusCode/100 != 2 || json.Unmarshal(data, &reg) != nil || reg.ClientID == "" {
		return "", fmt.Errorf(
			"login: registering the client: HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(data)),
		)
	}

	return reg.ClientID, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return err
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("login: %s: HTTP %d", u, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("login: %s: %w", u, err)
	}

	return nil
}

func postForm(ctx context.Context, hc *http.Client, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	return hc.Do(req)
}

// secure reports whether u is https, or http on this machine.
func secure(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}

	return p.Scheme == "https" || (p.Scheme == "http" && IsLoopback(p.Hostname()))
}

// IsLoopback reports whether host names this machine.
func IsLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)

	return base64.RawURLEncoding.EncodeToString(b)
}
