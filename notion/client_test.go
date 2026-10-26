package notion_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/amberpixels/f10-drivers/notion"
)

func TestClientRetriesOnceOnRateLimit(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Notion-Version") != notion.APIVersion {
			t.Errorf("Notion-Version = %q", r.Header.Get("Notion-Version"))
		}

		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write([]byte(`{"id":"p1","url":"https://x.test/p1"}`))
	}))
	defer srv.Close()

	p, err := notion.NewClient(srv.URL, "tok").Page(t.Context(), "p1")
	if err != nil || p.URL != "https://x.test/p1" || calls.Load() != 2 {
		t.Fatalf("page = %+v, err = %v, calls = %d", p, err, calls.Load())
	}
}

func TestClientRetriesOnServerError(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		var calls atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)

				return
			}

			_, _ = w.Write([]byte(`{"id":"p1","url":"https://x.test/p1"}`))
		}))

		p, err := notion.NewClient(srv.URL, "tok").Page(t.Context(), "p1")
		srv.Close()

		if err != nil || p.URL != "https://x.test/p1" || calls.Load() != 2 {
			t.Fatalf("%d: page = %+v, err = %v, calls = %d", status, p, err, calls.Load())
		}
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"object":"error","code":"object_not_found","message":"Could not find page."}`))
	}))
	defer srv.Close()

	_, err := notion.NewClient(srv.URL, "tok").Page(t.Context(), "p1")
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls, want object_not_found after 1", err, calls.Load())
	}
}

func TestClientGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"object":"error","code":"rate_limited","message":"Slow down."}`))
	}))
	defer srv.Close()

	_, err := notion.NewClient(srv.URL, "tok").Page(t.Context(), "p1")
	if err == nil || calls.Load() != 4 {
		t.Fatalf("err = %v after %d calls, want rate_limited after 4", err, calls.Load())
	}
}

func TestClientReportsNotionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"object":"error","code":"rate_limited","message":"Slow down."}`))
	}))
	defer srv.Close()

	_, err := notion.NewClient(srv.URL, "tok").Page(t.Context(), "p1")
	if err == nil || err.Error() != "notion: rate_limited: Slow down." {
		t.Fatalf("err = %v", err)
	}
}
