package notion

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestFromEventsSkipsNotifications(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\n" +
		"data: \"result\":{\"ok\":true}}\n\n"

	resp, err := fromEvents([]byte(stream), 7)
	if err != nil || string(resp.Result) != `{"ok":true}` {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}

	if _, err := fromEvents([]byte("data: {\"id\":1,\"result\":{}}\n\n"), 2); err == nil {
		t.Fatal("want an error when no event answers the request")
	}
}

func TestGroupOptionsKeepsNotionsOrder(t *testing.T) {
	raw := json.RawMessage(
		`{"to_do":[{"name":"Not started"}],"in_progress":[{"name":"Doing"},{"name":"Review"}],"complete":[{"name":"Done"}]}`,
	)

	var names []string
	for _, o := range groupOptions(raw) {
		names = append(names, o.Name)
	}

	if !slices.Equal(names, []string{"Not started", "Doing", "Review", "Done"}) {
		t.Fatalf("options = %v", names)
	}
}

// TestPageReadsRowsAndFetches reads both value shapes: a row's JSON-array
// strings and bare Unique ID, a fetch's arrays and prefixed one, the created
// time from a created_time property, and "ID" keyed as userDefined:ID.
func TestPageReadsRowsAndFetches(t *testing.T) {
	m := &mcpBackend{
		prefix: "MKT", idProp: "ID", created: "Created",
		users: map[string]string{"u1": "Ann Lee"},
		schema: map[string]PropertySchema{
			"Name": {Type: "title"}, "ID": {Type: "unique_id"}, "Status": {Type: "status"},
			"Tags": {Type: "multi_select"}, "Assignee": {Type: "people"}, "Blocked by": {Type: "relation"},
			"Created": {Type: "created_time"},
		},
	}

	const url = "https://app.notion.com/p/2b000000000040008000000000000002?pvs=204"

	row := map[string]any{
		"Name": "Draft", "userDefined:ID": "2", "Status": "Done", "Tags": `["report","data"]`,
		"Assignee": `["user://u1"]`, "Blocked by": `["https://app.notion.com/p/2b000000000040008000000000000001"]`,
		"Created": "2026-10-12T10:30:00.000Z",
	}
	fetched := map[string]any{
		"Name":           "Draft",
		"userDefined:ID": "MKT-2",
		"Status":         "Done",
		"Tags":           []any{"report", "data"},
		"Assignee": []any{
			"user://u1",
		},
		"Blocked by": []any{"https://app.notion.com/p/2b000000000040008000000000000001"},
		"Created":    "2026-10-12T10:30:00.000Z",
	}

	for name, values := range map[string]map[string]any{"row": row, "fetch": fetched} {
		p, err := m.page(t.Context(), url, values)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		got := []any{
			p.ID, p.URL, p.CreatedTime, *p.Properties["ID"].UniqueID.Number, p.Properties["Status"].Status.Name,
			len(p.Properties["Tags"].MultiSelect), p.Properties["Assignee"].People[0].Name,
			p.Properties["Blocked by"].Relation[0].ID,
		}
		want := []any{
			"2b000000-0000-4000-8000-000000000002", "https://app.notion.com/p/2b000000000040008000000000000002",
			"2026-10-12T10:30:00.000Z", 2, "Done", 2, "Ann Lee", "2b000000-0000-4000-8000-000000000001",
		}

		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, want)
		}
	}

	fetched["userDefined:ID"] = "WS-2"
	if _, err := m.page(t.Context(), url, fetched); err == nil {
		t.Error("want a prefix mismatch to fail")
	}
}

func TestCreatedID(t *testing.T) {
	for _, raw := range []string{
		`{"pages":[{"id":"2b000000-0000-4000-8000-000000000006","url":"https://www.notion.so/x"}]}`,
		`{"pages":[{"url":"https://app.notion.com/p/2b000000000040008000000000000006"}]}`,
		`{"result":"Created https://app.notion.com/p/2b000000000040008000000000000006"}`,
	} {
		if id, ok := createdID(json.RawMessage(raw)); !ok || id != "2b000000000040008000000000000006" {
			t.Errorf("createdID(%s) = %q, %v", raw, id, ok)
		}
	}
}

// TestShortErrorKeepsOneLine: Notion's tool errors append a JSON payload
// of recovery options meant for an assistant; the driver prints the code,
// the message and when to retry.
func TestShortErrorKeepsOneLine(t *testing.T) {
	text := "Your workspace has used up its allowance. Learn more: https://app.notion.com/x\n\n" +
		`{"error":{"code":"usage_limit_reached","message":"The Query Data Source usage limit has been reached.",` +
		`"impact":"The requested query was not executed.","retryability":"later","recovery_options":[]}}`

	want := "usage_limit_reached: The Query Data Source usage limit has been reached. " +
		"The requested query was not executed. Try again later."
	if got := shortError(text); got != want {
		t.Errorf("shortError = %q, want %q", got, want)
	}

	if got := shortError("Could not find page"); got != "Could not find page" {
		t.Errorf("plain text changed: %q", got)
	}
}
