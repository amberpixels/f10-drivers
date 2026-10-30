package notiontest

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The MCP face serves the fixture database as the hosted MCP server would:
// JSON-RPC over /mcp behind OAuth, each tool answering in the shape the
// backend parses, rows-mode filters evaluated rather than replayed.

// oauthState is the fake authorization server's: registered clients, codes
// waiting for exchange, and the tokens it issued.
type oauthState struct {
	n       int
	clients map[string][]string // client id to redirect uris
	codes   map[string]grant
	access  map[string]bool
	refresh map[string]string // refresh token to client id
	authz   int
}

type grant struct{ client, redirect, challenge string }

// MCPURL is the fake's MCP endpoint, the url a login and the backend use.
func (s *Server) MCPURL() string { return s.URL + "/mcp" }

// ExpireAccess makes every issued access token answer 401; refresh works.
func (s *Server) ExpireAccess() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.oauth.access = map[string]bool{}
}

// RevokeRefresh revokes every token: refresh answers invalid_grant.
func (s *Server) RevokeRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.oauth.access, s.oauth.refresh = map[string]bool{}, map[string]string{}
}

// EndSession ends the MCP session, as a server restart does: the next call
// carrying it answers 404.
func (s *Server) EndSession() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.session = ""
}

// Sessions counts the MCP sessions started.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.sessions
}

// Authorizations counts the requests to the authorize endpoint.
func (s *Server) Authorizations() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.oauth.authz
}

// ToolCalls counts the calls of one MCP tool.
func (s *Server) ToolCalls(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls[tool]
}

// serveMCP answers the MCP and OAuth paths, and reports whether it did.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp":
		reply(w, map[string]any{
			"resource": s.MCPURL(), "authorization_servers": []string{s.URL}, "scopes_supported": []string{"default"},
		})
	case "/.well-known/oauth-authorization-server":
		reply(w, map[string]any{
			"issuer":                                         s.URL,
			"authorization_endpoint":                         s.URL + "/authorize",
			"token_endpoint":                                 s.URL + "/token",
			"registration_endpoint":                          s.URL + "/register",
			"revocation_endpoint":                            s.URL + "/revoke",
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"authorization_response_iss_parameter_supported": true,
		})
	case "/register":
		s.register(w, r)
	case "/authorize":
		s.authorize(w, r)
	case "/token":
		s.token(w, r)
	case "/revoke":
		delete(s.oauth.refresh, r.FormValue("token"))
	case "/mcp":
		s.mcp(w, r)
	default:
		return false
	}

	return true
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}

	if err := json.NewDecoder(r.Body).
		Decode(&body); err != nil || len(body.RedirectURIs) == 0 ||
		body.AuthMethod != "none" {
		oauthError(w, "invalid_client_metadata")

		return
	}

	s.oauth.n++
	id := fmt.Sprintf("client-%d", s.oauth.n)
	s.oauth.clients[id] = body.RedirectURIs

	w.WriteHeader(http.StatusCreated)
	reply(w, map[string]any{"client_id": id, "redirect_uris": body.RedirectURIs})
}

// authorize stands in for the user consenting: it redirects straight back
// with a code, as Notion does after the user approves.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	s.oauth.authz++
	q := r.URL.Query()

	redirect := q.Get("redirect_uri")
	if !slices.Contains(s.oauth.clients[q.Get("client_id")], redirect) {
		http.Error(w, "unknown client or redirect_uri", http.StatusBadRequest)

		return
	}

	back, _ := url.Parse(redirect)
	answer := url.Values{"state": {q.Get("state")}, "iss": {s.URL}}

	switch {
	case q.Get("response_type") != "code", q.Get("code_challenge_method") != "S256", q.Get("code_challenge") == "":
		answer.Set("error", "invalid_request")
	case q.Get("resource") != s.MCPURL():
		answer.Set("error", "invalid_target")
	default:
		s.oauth.n++
		code := fmt.Sprintf("code-%d", s.oauth.n)
		s.oauth.codes[code] = grant{client: q.Get("client_id"), redirect: redirect, challenge: q.Get("code_challenge")}
		answer.Set("code", code)
	}

	back.RawQuery = answer.Encode()
	w.Header().Set("Location", back.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	client := r.FormValue("client_id")

	if r.FormValue("resource") != s.MCPURL() {
		oauthError(w, "invalid_target")

		return
	}

	switch r.FormValue("grant_type") {
	case "authorization_code":
		g, ok := s.oauth.codes[r.FormValue("code")]
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))

		if !ok || g.client != client || g.redirect != r.FormValue("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, "invalid_grant")

			return
		}

		delete(s.oauth.codes, r.FormValue("code"))
	case "refresh_token":
		if s.oauth.refresh[r.FormValue("refresh_token")] != client {
			oauthError(w, "invalid_grant")

			return
		}

		delete(s.oauth.refresh, r.FormValue("refresh_token"))
	default:
		oauthError(w, "unsupported_grant_type")

		return
	}

	s.oauth.n++
	access, refresh := fmt.Sprintf("access-%d", s.oauth.n), fmt.Sprintf("refresh-%d", s.oauth.n)
	s.oauth.access[access] = true
	s.oauth.refresh[refresh] = client

	reply(
		w,
		map[string]any{"access_token": access, "token_type": "bearer", "expires_in": 3600, "refresh_token": refresh},
	)
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	if !s.oauth.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="OAuth", resource_metadata="`+s.URL+`/.well-known/oauth-protected-resource/mcp", scope="default"`)
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	var req struct {
		ID     *int            `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != s.session {
		w.WriteHeader(http.StatusNotFound) // an ended session, as the spec answers it

		return
	}

	switch {
	case req.ID == nil:
		w.WriteHeader(http.StatusAccepted)
	case req.Method == "initialize":
		// Answered as an event stream, as the hosted server may answer any call.
		s.sessions++
		s.session = fmt.Sprintf("session-%d", s.sessions)
		w.Header().Set("Mcp-Session-Id", s.session)
		w.Header().Set("Content-Type", "text/event-stream")

		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{
			"protocolVersion": cmp.Or(s.Protocol, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "notiontest", "version": "0"},
		}})
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	case req.Method == "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}

		_ = json.Unmarshal(req.Params, &call)
		s.calls[call.Name]++

		if call.Name == "notion-create-pages" {
			s.creates++

			if s.CreateStatus != 0 {
				http.Error(w, "Notion is unavailable.", s.CreateStatus)

				return
			}
		}

		status := 0

		switch call.Name {
		case "notion-update-page":
			s.updates++
			if s.updates == 1 {
				status = s.UpdateStatus
			}
		case "notion-create-comment":
			s.posts++
			if s.posts == 1 {
				status = s.CommentStatus
			}
		}

		if status != 0 {
			http.Error(w, "Notion is unavailable.", status)

			return
		}

		result, isErr, known := s.tool(call.Name, call.Arguments)
		if !known {
			rpcReply(w, *req.ID, nil, map[string]any{"code": -32602, "message": "Unknown tool: " + call.Name})

			return
		}

		text, _ := result.(string)
		if !isErr {
			data, _ := json.Marshal(result)
			text = string(data)
		}

		rpcReply(
			w,
			*req.ID,
			map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr},
			nil,
		)
	default:
		rpcReply(w, *req.ID, nil, map[string]any{"code": -32601, "message": "Method not found"})
	}
}

func rpcReply(w http.ResponseWriter, id int, result, rpcErr any) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}

	reply(w, msg)
}

// tool answers one tool call: the result, whether it is a tool error (then
// a string), and whether the tool exists.
func (s *Server) tool(name string, raw json.RawMessage) (any, bool, bool) {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)

	switch name {
	case "notion-fetch":
		id, _ := args["id"].(string)
		res, err := s.fetch(id)

		return orText(res, err)
	case "notion-query-data-sources":
		data, _ := args["data"].(map[string]any)
		res, err := s.rows(data)

		return orText(res, err)
	case "notion-get-comments":
		id, _ := args["page_id"].(string)

		return map[string]any{"text": s.discussions(norm(id))}, false, true
	case "notion-get-users":
		return s.getUsers(args), false, true
	case "notion-update-page":
		res, err := s.updatePage(args)

		return orText(res, err)
	case "notion-create-comment":
		res, err := s.createComment(args)

		return orText(res, err)
	case "notion-create-pages":
		res, err := s.createPages(args)

		return orText(res, err)
	}

	return nil, false, false
}

func orText(res any, err error) (any, bool, bool) {
	if err != nil {
		return err.Error(), true, true
	}

	return res, false, true
}

func (s *Server) fetch(id string) (any, error) {
	if ds, ok := strings.CutPrefix(id, "collection://"); ok {
		if norm(ds) != DataSourceID {
			return nil, notionErrorf("Could not find data source with ID: %s", id)
		}

		state, _ := json.Marshal(map[string]any{"name": "Marketing tasks", "schema": s.mcpSchema()})

		return map[string]any{
			"metadata": map[string]any{"type": "data_source"},
			"title":    "Marketing tasks",
			"url":      s.databaseURL() + "?pvs=204",
			"text": fmt.Sprintf(
				"<data-source url=\"{{collection://%s}}\">\nThe title of this Data Source is: Marketing tasks\n\n"+
					"<data-source-state>\n%s\n</data-source-state>\n</data-source>",
				dashed(DataSourceID),
				state,
			),
		}, nil
	}

	if norm(id) == DatabaseID {
		return map[string]any{
			"metadata": map[string]any{"type": "database"},
			"title":    "Marketing tasks",
			"url":      s.databaseURL() + "?pvs=204",
			"text": fmt.Sprintf("<database url=\"%s\">\n<data-sources>\n<data-source url=\"{{collection://%s}}\">\n"+
				"The title of this Data Source is: Marketing tasks\n</data-source>\n</data-sources>\n</database>",
				s.databaseURL(), dashed(DataSourceID)),
		}, nil
	}

	p := s.pageByID(norm(id))
	if p == nil {
		return nil, notionErrorf("Could not find page with ID: %s", id)
	}

	pid, _ := p["id"].(string)
	purl := appURL(pid)
	props, _ := json.Marshal(s.values(p, true))

	md, _ := s.markdown[norm(pid)].(map[string]any)
	body, _ := md["markdown"].(string)

	res := map[string]any{
		"metadata": map[string]any{"type": "page"},
		"url":      purl + "?pvs=204",
		"text": fmt.Sprintf(
			"Here is the result of \"fetch\" for the Page with URL %s:\n<page url=\"%s\">\n<ancestor-path>\n"+
				"<parent-data-source url=\"collection://%s\" name=\"Marketing tasks\"/>\n"+
				"<ancestor-2-database url=\"%s\" title=\"Marketing tasks\"/>\n</ancestor-path>\n"+
				"<properties>\n%s\n</properties>\n<content>\n%s\n</content>\n</page>",
			purl,
			purl,
			dashed(DataSourceID),
			s.databaseURL(),
			props,
			body,
		),
	}

	if t, _ := md["truncated"].(bool); t {
		res["truncated"] = true
	}

	if ids, _ := md["unknown_block_ids"].([]any); len(ids) > 0 {
		res["unknown_block_ids"] = ids
		res["unknown_block_count"] = len(ids)
	}

	return res, nil
}

func (s *Server) databaseURL() string { return appURL(DatabaseID) }

// valueKey is a property's key in tool values: "id" and "url", in any
// case, take a userDefined: prefix, clear of the page's own url.
func valueKey(name string) string {
	if l := strings.ToLower(name); l == "id" || l == "url" {
		return "userDefined:" + name
	}

	return name
}

// appURL is a page or database url as the MCP tools give it.
func appURL(id string) string { return "https://app.notion.com/p/" + norm(id) }

// mcpSchema renders the schema as <data-source-state> holds it: MCP's type
// words, no Unique ID prefix, a status property's options in groups.
func (s *Server) mcpSchema() map[string]any {
	schema := s.schema()
	out := map[string]any{}

	for name, typ := range schemaTypes(schema) {
		p := map[string]any{"name": name, "description": "", "type": map[string]string{
			"unique_id": "auto_increment_id", "people": "person", "rich_text": "text",
		}[typ]}
		if p["type"] == "" {
			p["type"] = typ
		}

		opts := []any{}
		for _, o := range options(schema, name, typ) {
			opts = append(opts, map[string]any{"name": o, "color": "default"})
		}

		switch typ {
		case "status":
			groups := map[string]any{
				"to_do":       opts[:1],
				"in_progress": opts[1 : len(opts)-1],
				"complete":    opts[len(opts)-1:],
			}
			p["groups"] = groups
		case "select", "multi_select":
			p["options"] = opts
		}

		out[valueKey(name)] = p
	}

	return out
}

// values renders a page's properties as the MCP tools show them: a fetch
// with arrays and the prefixed Unique ID, a row with JSON-array strings and
// the bare number.
func (s *Server) values(p page, fetch bool) map[string]any {
	props, _ := p["properties"].(map[string]any)
	out := map[string]any{}

	list := func(items []string) any {
		if fetch {
			return items
		}

		if len(items) == 0 {
			return ""
		}

		data, _ := json.Marshal(items)

		return string(data)
	}

	for name, raw := range props {
		prop, _ := raw.(map[string]any)
		typ, _ := prop["type"].(string)

		name = valueKey(name)

		switch typ {
		case "title":
			out[name] = text(prop["title"])
		case "unique_id":
			uid, _ := prop["unique_id"].(map[string]any)
			n, _ := uid["number"].(float64)
			out[name] = strconv.Itoa(int(n))

			if pre, _ := uid["prefix"].(string); fetch && pre != "" {
				out[name] = pre + "-" + strconv.Itoa(int(n))
			}
		case "status", "select":
			opt, _ := prop[typ].(map[string]any)
			out[name], _ = opt["name"].(string)
		case "url", "email", "phone_number", "number":
			if v := prop[typ]; v != nil {
				out[name] = v
			}
		case "rich_text":
			if t := text(prop[typ]); t != "" {
				out[name] = t
			}
		case "checkbox":
			if b, ok := prop[typ].(bool); ok {
				out[name] = map[bool]string{true: "__YES__", false: "__NO__"}[b]
			}
		case "date":
			if d, ok := prop[typ].(map[string]any); ok {
				out["date:"+name+":start"] = d["start"]
				out["date:"+name+":end"] = d["end"]
				out["date:"+name+":is_datetime"] = map[bool]int{true: 1, false: 0}[len(fmt.Sprint(d["start"])) > 10]
			}
		case "multi_select", "people", "relation":
			var items []string

			vals, _ := prop[typ].([]any)
			for _, v := range vals {
				vm, _ := v.(map[string]any)

				switch typ {
				case "multi_select":
					n, _ := vm["name"].(string)
					items = append(items, n)
				case "people":
					id, _ := vm["id"].(string)
					items = append(items, "user://"+id)
				default:
					id, _ := vm["id"].(string)
					items = append(items, appURL(id))
				}
			}

			out[name] = list(items)
		}
	}

	id, _ := p["id"].(string)
	out["url"] = appURL(id)

	return out
}

// rows answers rows mode: filters evaluated, has_more past the limit.
func (s *Server) rows(data map[string]any) (any, error) {
	if data["mode"] != "rows" {
		return nil, fmt.Errorf("the fake answers rows mode only, got %v", data["mode"])
	}

	dsURL, _ := data["data_source_url"].(string)
	if norm(strings.TrimPrefix(dsURL, "collection://")) != DataSourceID {
		return nil, notionErrorf("Could not find data source: %s", dsURL)
	}

	limit := 50

	if l, ok := data["limit"].(float64); ok {
		limit = int(l)
	}

	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100, got %d", limit)
	}

	if s.RowLimit > 0 {
		limit = min(limit, s.RowLimit)
	}

	filter, _ := data["filter"].(map[string]any)
	results := []any{}

	for _, p := range s.pages {
		ok, err := s.rowMatch(p, filter)
		if err != nil {
			return nil, err
		}

		if ok {
			results = append(results, s.values(p, false))
		}
	}

	more := len(results) > limit

	return map[string]any{"results": results[:min(limit, len(results))], "has_more": more}, nil
}

// rowMatch evaluates a rows-mode filter: a group of property filters, each
// checked against the schema's MCP type.
func (s *Server) rowMatch(p page, f map[string]any) (bool, error) {
	if f == nil {
		return true, nil
	}

	if f["type"] == "group" {
		subs, _ := f["filters"].([]any)
		and := f["operator"] != "or"

		for _, sub := range subs {
			sm, _ := sub.(map[string]any)

			ok, err := s.rowMatch(p, sm)
			if err != nil {
				return false, err
			}

			if ok != and {
				return ok, nil
			}
		}

		return and, nil
	}

	name, _ := f["property"].(string)
	ptype, _ := f["propertyType"].(string)
	op, _ := f["operator"].(string)
	val, _ := f["value"].(map[string]any)

	schema, _ := s.mcpSchema()[name].(map[string]any)
	if schema == nil {
		return true, nil // Notion drops a filter on a property it does not know
	}

	if schema["type"] != ptype {
		return false, fmt.Errorf("propertyType %q does not match %q, a %v property", ptype, name, schema["type"])
	}

	row := s.values(p, true)
	key := name
	want := val["value"]

	switch op {
	case "number_equals":
		uid, _ := s.values(p, false)[key].(string)
		n, _ := want.(float64)

		return val["type"] == "exact" && uid == strconv.Itoa(int(n)), nil
	case "status_is", "status_is_not":
		if val["type"] != "is_option" {
			return false, errors.New("status filters take is_option values")
		}

		return (row[key] == want) == (op == "status_is"), nil
	case "enum_is", "enum_is_not":
		return (row[key] == want) == (op == "enum_is"), nil
	case "string_contains":
		got, _ := row[key].(string)
		w, _ := want.(string)

		return strings.Contains(strings.ToLower(got), strings.ToLower(w)), nil
	case "relation_contains":
		refs, _ := row[key].([]string)
		w, _ := want.(string)

		return slices.ContainsFunc(refs, func(r string) bool { return strings.HasSuffix(r, norm(w)) }), nil
	}

	return false, fmt.Errorf("the fake has no operator %q", op)
}

// discussions renders a page's comments as notion-get-comments does:
// page-level discussions, authors as user:// urls with no name.
func (s *Server) discussions(pageID string) string {
	var b strings.Builder

	comments := s.comments[pageID]
	fmt.Fprintf(
		&b,
		"<discussions total-count=\"%d\" shown-count=\"%d\">\n",
		min(len(comments), 1),
		min(len(comments), 1),
	)

	if len(comments) > 0 {
		fmt.Fprintf(
			&b,
			"<discussion id=\"discussion://%s/%s/d1\" comment-count=\"%d\" resolved=\"false\" type=\"comment\" context=\"page\">\n",
			dashed(pageID),
			dashed(pageID),
			len(comments),
		)

		for _, c := range comments {
			cm, _ := c.(map[string]any)
			by, _ := cm["created_by"].(map[string]any)
			uid, _ := by["id"].(string)
			at, _ := cm["created_time"].(string)
			id, _ := cm["id"].(string)

			fmt.Fprintf(
				&b,
				"<comment id=\"%s\" url=\"https://www.notion.so/%s?d=d1&amp;pvs=42\" user-url=\"user://%s\" datetime=\"%s\">%s</comment>\n",
				id,
				pageID,
				uid,
				at,
				html.EscapeString(text(cm["rich_text"])),
			)
		}

		b.WriteString("</discussion>\n")
	}

	b.WriteString("</discussions>")

	return b.String()
}

// getUsers answers one user by user_id, or the people whose name or email
// holds query, with their emails.
func (s *Server) getUsers(args map[string]any) any {
	id, _ := args["user_id"].(string)
	query, _ := args["query"].(string)
	results := []any{}

	if id != "" {
		if n, ok := s.users()[id]; ok {
			results = append(results, map[string]any{"type": "person", "id": id, "name": n})
		}

		return map[string]any{"results": results, "has_more": false}
	}

	for _, u := range s.people {
		um, _ := u.(map[string]any)
		person, _ := um["person"].(map[string]any)
		name, _ := um["name"].(string)
		email, _ := person["email"].(string)

		q := strings.ToLower(query)
		if strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(email), q) {
			results = append(results, map[string]any{"type": um["type"], "id": um["id"], "name": name, "email": email})
		}
	}

	return map[string]any{"results": results, "has_more": false}
}

// updatePage answers update_properties with values in their SQLite form,
// checked as createPages checks them.
func (s *Server) updatePage(args map[string]any) (any, error) {
	if cmd, _ := args["command"].(string); cmd != "update_properties" {
		return nil, fmt.Errorf("the fake updates properties only, got %q", cmd)
	}

	if args["allow_async"] != false {
		return nil, errors.New("the fake updates synchronously only")
	}

	pid, _ := args["page_id"].(string)

	p := s.pageByID(norm(pid))
	if p == nil {
		return nil, notionErrorf("Could not find page with ID: %s", pid)
	}

	values, _ := args["properties"].(map[string]any)
	schema := s.schema()
	types := schemaTypes(schema)
	set := map[string]any{}

	for key, v := range values {
		if strings.HasPrefix(key, "date:") {
			parts := strings.Split(key, ":")
			if len(parts) != 3 || types[parts[1]] != "date" {
				return nil, notionErrorf("Property %q does not exist", key)
			}

			// Each key changes its own field; a range keeps an end not sent.
			d, _ := set[parts[1]].(map[string]any)
			if d == nil {
				d = s.dateOf(p, parts[1])
			}

			if parts[2] == "start" || parts[2] == "end" {
				d[parts[2]] = v
			}

			set[parts[1]] = d

			continue
		}

		name := strings.TrimPrefix(key, "userDefined:")
		if valueKey(name) != key {
			return nil, notionErrorf("Property %q must be written %q", key, valueKey(name))
		}

		typ, ok := types[name]
		if !ok {
			return nil, notionErrorf("Property %q does not exist", name)
		}

		req, err := sqliteToRequest(typ, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		resp, err := s.respond(typ, req, options(schema, name, typ))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		set[name] = resp
	}

	for name, v := range set {
		if d, ok := v.(map[string]any); ok && types[name] == "date" && d["start"] == nil {
			set[name] = nil
		}
	}

	s.apply(p, set)

	return map[string]any{"page_id": pid}, nil
}

// dateOf copies a page's date value, or an empty one.
func (s *Server) dateOf(p page, name string) map[string]any {
	props, _ := p["properties"].(map[string]any)
	prop, _ := props[name].(map[string]any)
	d, _ := prop["date"].(map[string]any)

	out := map[string]any{"start": nil, "end": nil, "time_zone": nil}
	maps.Copy(out, d)

	return out
}

// sqliteToRequest turns a create's or an update's SQLite value into the
// REST request shape respond checks.
func sqliteToRequest(typ string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}

	switch typ {
	case "status", "select":
		return map[string]any{"name": v}, nil
	case "checkbox":
		switch v {
		case "__YES__":
			return true, nil
		case "__NO__":
			return false, nil
		}

		return nil, fmt.Errorf("a checkbox takes __YES__ or __NO__, got %v", v)
	case "number":
		if _, ok := v.(float64); !ok {
			return nil, fmt.Errorf("a number takes a JSON number, got %v", v)
		}

		return v, nil
	case "url", "email", "phone_number":
		return v, nil
	case "rich_text":
		return []any{map[string]any{"text": map[string]any{"content": v}}}, nil
	case "people":
		items, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("a person property takes an array, got %v", v)
		}

		out := make([]any, 0, len(items))
		for _, item := range items {
			id, _ := item.(string)
			out = append(out, map[string]any{"id": strings.TrimPrefix(id, "user://")})
		}

		return out, nil
	case "multi_select":
		items, _ := v.([]any)
		out := make([]any, 0, len(items))

		for _, item := range items {
			out = append(out, map[string]any{"name": item})
		}

		return out, nil
	}

	return nil, fmt.Errorf("the fake cannot update a %s property", typ)
}

// createComment stores a comment from rich text, as Notion stores it.
func (s *Server) createComment(args map[string]any) (any, error) {
	pid, _ := args["page_id"].(string)
	if s.pageByID(norm(pid)) == nil {
		return nil, notionErrorf("Could not find page with ID: %s", pid)
	}

	if _, ok := args["markdown"]; ok {
		return nil, errors.New("the fake takes rich_text")
	}

	items, _ := args["rich_text"].([]any)
	if len(items) == 0 || len(items) > 100 {
		return nil, notionErrorf("rich_text should have 1 to 100 items")
	}

	runs := make([]any, 0, len(items))

	for _, item := range items {
		im, _ := item.(map[string]any)
		txt, _ := im["text"].(map[string]any)
		runs = append(runs, map[string]any{"type": "text", "plain_text": txt["content"]})
	}

	c := s.addComment(norm(pid), runs)

	return map[string]any{"id": c["id"]}, nil
}

// users is everyone the fixtures name: people properties, and comment
// authors whose name Notion resolves.
func (s *Server) users() map[string]string {
	users := map[string]string{}

	for _, p := range s.pages {
		props, _ := p["properties"].(map[string]any)

		for _, raw := range props {
			prop, _ := raw.(map[string]any)
			people, _ := prop["people"].([]any)

			for _, person := range people {
				pm, _ := person.(map[string]any)
				id, _ := pm["id"].(string)
				users[id], _ = pm["name"].(string)
			}
		}
	}

	for _, u := range s.people {
		um, _ := u.(map[string]any)
		id, _ := um["id"].(string)
		users[id], _ = um["name"].(string)
	}

	for _, cs := range s.comments {
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			by, _ := cm["created_by"].(map[string]any)
			dn, _ := cm["display_name"].(map[string]any)

			if id, _ := by["id"].(string); dn["resolved_name"] != nil {
				users[id], _ = dn["resolved_name"].(string)
			}
		}
	}

	return users
}

// createPages takes properties in their SQLite value form and checks
// options against the schema, as Notion does.
func (s *Server) createPages(args map[string]any) (any, error) {
	parent, _ := args["parent"].(map[string]any)
	if ds, _ := parent["data_source_id"].(string); norm(ds) != DataSourceID {
		return nil, notionErrorf("Could not find data source with ID: %s", ds)
	}

	if args["allow_async"] != false {
		return nil, errors.New("the fake creates synchronously only")
	}

	pages, _ := args["pages"].([]any)
	if len(pages) != 1 {
		return nil, fmt.Errorf("the fake creates one page at a time, got %d", len(pages))
	}

	pg, _ := pages[0].(map[string]any)
	props, _ := pg["properties"].(map[string]any)
	content, _ := pg["content"].(string)

	schema := s.schema()
	types := schemaTypes(schema)
	set := map[string]any{}

	for key, v := range props {
		if strings.HasPrefix(key, "date:") {
			parts := strings.Split(key, ":")
			if len(parts) != 3 || types[parts[1]] != "date" {
				return nil, notionErrorf("Property %q does not exist", key)
			}

			d, _ := set[parts[1]].(map[string]any)
			if d == nil {
				d = map[string]any{"start": nil, "end": nil, "time_zone": nil}
			}

			if parts[2] == "start" || parts[2] == "end" {
				d[parts[2]] = v
			}

			set[parts[1]] = d

			continue
		}

		name := strings.TrimPrefix(key, "userDefined:")
		if valueKey(name) != key {
			return nil, notionErrorf("Property %q must be written %q", key, valueKey(name))
		}

		typ, ok := types[name]
		if !ok {
			return nil, notionErrorf("Property %q does not exist", name)
		}

		var req any

		switch typ {
		case "title":
			t, _ := v.(string)
			req = []any{map[string]any{"text": map[string]any{"content": t}}}
		case "status", "select":
			req = map[string]any{"name": v}
		case "multi_select", "relation":
			items, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: a %s property takes an array", name, typ)
			}

			out := make([]any, 0, len(items))

			for _, item := range items {
				if typ == "relation" {
					id, _ := item.(string)
					out = append(out, map[string]any{"id": dashed(norm(lastID.FindString(id)))})

					continue
				}

				out = append(out, map[string]any{"name": item})
			}

			req = out
		default:
			r, err := sqliteToRequest(typ, v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}

			req = r
		}

		resp, err := s.respond(typ, req, options(schema, name, typ))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}

		set[name] = resp
	}

	p := s.insert(set, content)

	id, _ := p["id"].(string)

	return map[string]any{"pages": []any{map[string]any{"id": id, "url": appURL(id)}}}, nil
}

// notionError is a tool error worded as Notion words it.
type notionError string

func (t notionError) Error() string { return string(t) }

func notionErrorf(format string, args ...any) error { return notionError(fmt.Sprintf(format, args...)) }

var lastID = regexp.MustCompile(`[0-9a-fA-F-]{32,36}$`)

func dashed(id string) string {
	n := norm(id)
	if len(n) != 32 {
		return id
	}

	return n[:8] + "-" + n[8:12] + "-" + n[12:16] + "-" + n[16:20] + "-" + n[20:]
}
