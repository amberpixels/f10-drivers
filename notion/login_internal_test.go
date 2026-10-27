package notion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amberpixels/f10-drivers/config"
	"github.com/amberpixels/f10-drivers/notion/notiontest"
)

// isolate keeps the test's logins in a temp home, out of the keychain.
func isolate(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv(TokenStoreEnv, "file")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// approve stands in for the browser: it opens the authorize url, lets
// rewrite change the redirect back, and follows it to the callback.
func approve(t *testing.T, rewrite func(q url.Values)) func(string) error {
	t.Helper()

	noFollow := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	get := func(hc *http.Client, u string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
		if err != nil {
			return nil, err
		}

		return hc.Do(req)
	}

	return func(u string) error {
		resp, err := get(noFollow, u)
		if err != nil {
			return err
		}

		_ = resp.Body.Close()

		back, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			return err
		}

		if rewrite != nil {
			q := back.Query()
			rewrite(q)
			back.RawQuery = q.Encode()
		}

		resp, err = get(http.DefaultClient, back.String())
		if err != nil {
			return err
		}

		return resp.Body.Close()
	}
}

func loggedIn(t *testing.T) *notiontest.Server {
	t.Helper()
	isolate(t)

	srv := notiontest.New(t)
	if err := Login(
		t.Context(),
		LoginOptions{Server: srv.MCPURL(), Open: approve(t, nil), Out: io.Discard},
	); err != nil {
		t.Fatal(err)
	}

	return srv
}

func TestLoginRefusesAForeignAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(q url.Values)
		want    string
	}{
		{"state", func(q url.Values) { q.Set("state", "someone-elses") }, "state does not match"},
		{"iss", func(q url.Values) { q.Set("iss", "https://elsewhere.test") }, `comes from "https://elsewhere.test"`},
		{"no iss", func(q url.Values) { q.Del("iss") }, "comes from"},
		{"error", func(q url.Values) { q.Del("code"); q.Set("error", "access_denied") }, "Notion answered access_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)

			srv := notiontest.New(t)

			err := Login(t.Context(), LoginOptions{Server: srv.MCPURL(), Open: approve(t, tc.rewrite), Out: io.Discard})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}

			if _, err := LoadSession(t.Context(), srv.MCPURL()); !errors.Is(err, ErrNoLogin) {
				t.Fatalf("a refused login stored something: %v", err)
			}
		})
	}
}

// TestDiscoverRefuses serves metadata that breaks one rule at a time.
func TestDiscoverRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(res, as map[string]any)
		want string
	}{
		{"http endpoint", func(_, as map[string]any) { as["token_endpoint"] = "http://notion.test/token" }, "must be https"},
		{"no registration", func(_, as map[string]any) { delete(as, "registration_endpoint") }, "offers no client registration"},
		{"no S256", func(_, as map[string]any) { as["code_challenge_methods_supported"] = []string{"plain"} }, "PKCE S256"},
		{"other issuer", func(_, as map[string]any) { as["issuer"] = "https://elsewhere.test" }, "names issuer"},
		{"other resource", func(res, _ map[string]any) { res["resource"] = "https://elsewhere.test/mcp" }, "describes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server

			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				res := map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}}
				as := map[string]any{
					"issuer":                           srv.URL,
					"authorization_endpoint":           srv.URL + "/authorize",
					"token_endpoint":                   srv.URL + "/token",
					"registration_endpoint":            srv.URL + "/register",
					"code_challenge_methods_supported": []string{"S256"},
				}
				tc.edit(res, as)

				switch r.URL.Path {
				case "/mcp":
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/rm"`, srv.URL))
					w.WriteHeader(http.StatusUnauthorized)
				case "/rm":
					_ = json.NewEncoder(w).Encode(res)
				case "/.well-known/oauth-authorization-server":
					_ = json.NewEncoder(w).Encode(as)
				}
			}))
			defer srv.Close()

			_, err := discover(t.Context(), srv.Client(), srv.URL+"/mcp")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSessionRefreshesBeforeExpiry(t *testing.T) {
	srv := loggedIn(t)

	sess, err := LoadSession(t.Context(), srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}

	old := sess.login.Access
	sess.login.Expiry = time.Now().Add(refreshMargin / 2)

	tok, err := sess.Token(t.Context())
	if err != nil || tok == old {
		t.Fatalf("token = %q (was %q), err = %v; want a refreshed one", tok, old, err)
	}

	stored, err := LoadSession(t.Context(), srv.MCPURL())
	if err != nil || stored.login.Access != tok {
		t.Fatalf("the refreshed token was not stored: %v", err)
	}
}

// TestMCPBackendTakesConcurrentPageReads is prefetchAfter's load: three
// fetchers at once on an expired token, which must refresh once (the fake
// rotates refresh tokens, so a second refresh with the old one fails).
// Run it with -race.
func TestMCPBackendTakesConcurrentPageReads(t *testing.T) {
	srv := loggedIn(t)
	cfg := &config.Config{Prefix: "MKT", Notion: &config.Notion{Properties: config.Properties{ID: "ID"}}}

	m, err := newMCPBackend(t.Context(), cfg, srv.MCPURL(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.DataSource(t.Context(), notiontest.DataSourceID); err != nil {
		t.Fatal(err)
	}

	srv.ExpireAccess()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for range fetchers {
		wg.Go(func() {
			for n := 1; n <= 5; n++ {
				_, err := m.Page(t.Context(), fmt.Sprintf("2b000000-0000-4000-8000-%012d", n))

				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent reads failed: %v", errs)
	}
}

// TestRefreshTakesUpAnotherRunsToken: two runs hold the same login; the
// second refreshes after the first spent the refresh token, and must take
// the stored one up rather than report the login revoked.
func TestRefreshTakesUpAnotherRunsToken(t *testing.T) {
	srv := loggedIn(t)

	first, err := LoadSession(t.Context(), srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}

	second, err := LoadSession(t.Context(), srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}

	tok, err := first.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if got, err := second.Refresh(t.Context()); err != nil || got != tok {
		t.Fatalf("second refresh = %q, %v; want the first run's %q", got, err, tok)
	}
}

func mcpClientOn(t *testing.T, srv *notiontest.Server) *mcpClient {
	t.Helper()

	sess, err := LoadSession(t.Context(), srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}

	return newMCPClient(srv.MCPURL(), sess, "test")
}

func TestMCPClientRefusesAnUnknownProtocol(t *testing.T) {
	srv := loggedIn(t)
	srv.Protocol = "2099-01-01"

	var out any

	err := mcpClientOn(t, srv).call(t.Context(), toolUsers, map[string]any{"user_id": "u1"}, false, &out)
	if err == nil || !strings.Contains(err.Error(), `the server speaks MCP "2099-01-01"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPClientStartsANewSessionAfterAnEndedOne(t *testing.T) {
	srv := loggedIn(t)
	c := mcpClientOn(t, srv)

	var out any
	for i := range 2 {
		if i == 1 {
			srv.EndSession()
		}

		if err := c.call(t.Context(), toolUsers, map[string]any{"user_id": "u1"}, false, &out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if srv.Sessions() != 2 || srv.ToolCalls(toolUsers) != 2 {
		t.Fatalf("%d sessions and %d calls, want 2 of each", srv.Sessions(), srv.ToolCalls(toolUsers))
	}
}

func TestMCPClientKeepsTheServersErrorWords(t *testing.T) {
	srv := loggedIn(t)

	var out any

	err := mcpClientOn(t, srv).call(t.Context(), "notion-teleport", map[string]any{}, false, &out)
	if err == nil || !strings.Contains(err.Error(), `no tool "notion-teleport"`) ||
		!strings.Contains(err.Error(), "Unknown tool: notion-teleport") {
		t.Fatalf("err = %v", err)
	}
}

func mcpBackendOn(t *testing.T, srv *notiontest.Server) *mcpBackend {
	t.Helper()

	cfg := &config.Config{Prefix: "MKT", Notion: &config.Notion{Properties: config.Properties{ID: "ID"}}}

	m, err := newMCPBackend(t.Context(), cfg, srv.MCPURL(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.DataSource(t.Context(), notiontest.DataSourceID); err != nil {
		t.Fatal(err)
	}

	return m
}

// TestMCPQueryRefusesAnIgnoredFilter: Notion answers every row when a
// filter names a property it does not know; that must fail, not answer.
func TestMCPQueryRefusesAnIgnoredFilter(t *testing.T) {
	m := mcpBackendOn(t, loggedIn(t))

	_, err := m.Query(t.Context(), notiontest.DataSourceID, Query{
		Filters: []Filter{{Property: "Number", Type: "unique_id", Op: OpEquals, Value: 2}}, PageSize: 1,
	}, false)
	if err == nil || !strings.Contains(err.Error(), `ignored the filter on "Number"`) {
		t.Fatalf("err = %v", err)
	}

	pages, err := m.Query(t.Context(), notiontest.DataSourceID, Query{
		Filters: []Filter{{Property: "ID", Type: "unique_id", Op: OpEquals, Value: 2}}, PageSize: 1,
	}, false)
	if err != nil || len(pages) != 1 || *pages[0].Properties["ID"].UniqueID.Number != 2 {
		t.Fatalf("lookup of 2 = %+v, %v", pages, err)
	}
}

func TestMCPDatabaseNamesTheDatabaseOfATaskPage(t *testing.T) {
	m := mcpBackendOn(t, loggedIn(t))

	_, err := m.Database(t.Context(), "2b000000000040008000000000000002")
	if err == nil ||
		!strings.Contains(err.Error(), "is a page in the database https://app.notion.com/p/"+notiontest.DatabaseID) {
		t.Fatalf("err = %v", err)
	}
}
