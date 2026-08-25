package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func rateLimitHeaders(resource string, limit, remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Resource", resource)
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

func TestParseRateLimit(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute)
	h := rateLimitHeaders("graphql", 5000, 3120, reset)
	rl, ok := parseRateLimit(h)
	if !ok {
		t.Fatal("parseRateLimit returned ok=false")
	}
	if rl.Resource != "graphql" || rl.Limit != 5000 || rl.Remaining != 3120 {
		t.Fatalf("RateLimit = %+v, want graphql 3120/5000", rl)
	}
	if !rl.Reset.Equal(reset.Truncate(time.Second)) {
		t.Fatalf("Reset = %v, want %v", rl.Reset, reset)
	}
}

func TestParseRateLimitMissingReset(t *testing.T) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "30")
	h.Set("X-RateLimit-Remaining", "27")
	rl, ok := parseRateLimit(h)
	if !ok {
		t.Fatal("parseRateLimit returned ok=false")
	}
	if rl.Resource != "core" {
		t.Fatalf("missing resource should default to core, got %q", rl.Resource)
	}
	if !rl.Reset.IsZero() {
		t.Fatalf("missing reset should be zero, got %v", rl.Reset)
	}
}

func TestParseRateLimitNoHeaders(t *testing.T) {
	if _, ok := parseRateLimit(http.Header{}); ok {
		t.Fatal("headers without a limit should return ok=false")
	}
}

type roundTripFunc func(r *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func respWithRateLimit(resource string, limit, remaining int) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     rateLimitHeaders(resource, limit, remaining, time.Now().Add(time.Hour)),
		Body:       io.NopCloser(nopReader{}),
	}
}

// nopReader satisfies io.ReadCloser without a body so RoundTrip callers can
// close it.
type nopReader struct{}

func (nopReader) Read([]byte) (int, error) { return 0, io.EOF }
func (nopReader) Close() error             { return nil }

func TestCountingTransportCapturesRateLimits(t *testing.T) {
	responses := map[string]*http.Response{}
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responses[r.URL.Path], nil
	})
	ct := &countingTransport{base: base}
	c := &Client{ct: ct}

	responses["/search/issues"] = respWithRateLimit("search", 30, 27)
	if _, err := ct.RoundTrip(&http.Request{URL: &url.URL{Path: "/search/issues"}}); err != nil {
		t.Fatal(err)
	}
	responses["/graphql"] = respWithRateLimit("graphql", 5000, 4800)
	if _, err := ct.RoundTrip(&http.Request{URL: &url.URL{Path: "/graphql"}}); err != nil {
		t.Fatal(err)
	}

	rls := c.RateLimits()
	if len(rls) != 2 {
		t.Fatalf("RateLimits() = %+v, want 2 resources", rls)
	}
	if rls[0].Resource != "graphql" || rls[0].Limit != 5000 || rls[0].Remaining != 4800 {
		t.Fatalf("first = %+v, want graphql 4800/5000 (sorted)", rls[0])
	}
	if rls[1].Resource != "search" || rls[1].Limit != 30 || rls[1].Remaining != 27 {
		t.Fatalf("second = %+v, want search 27/30 (sorted)", rls[1])
	}
}

func TestRefreshRateLimits(t *testing.T) {
	body := `{"resources":{
		"core":{"limit":5000,"used":1337,"remaining":3663,"reset":1700000000},
		"search":{"limit":30,"used":3,"remaining":27,"reset":1700000000},
		"graphql":{"limit":5000,"used":200,"remaining":4800,"reset":1700000000},
		"code_scanning_upload":{"limit":500,"remaining":500,"reset":1700000000}
	}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c := &Client{
		hc:      &http.Client{Transport: http.DefaultTransport},
		ct:      &countingTransport{base: http.DefaultTransport},
		baseURL: srv.URL,
	}
	if err := c.RefreshRateLimits(context.Background()); err != nil {
		t.Fatal(err)
	}

	rls := c.RateLimits()
	if len(rls) != 3 {
		t.Fatalf("RateLimits() = %+v, want 3 resources (extra resources filtered)", rls)
	}
	want := []RateLimit{
		{Resource: "core", Limit: 5000, Remaining: 3663, Reset: time.Unix(1700000000, 0)},
		{Resource: "graphql", Limit: 5000, Remaining: 4800, Reset: time.Unix(1700000000, 0)},
		{Resource: "search", Limit: 30, Remaining: 27, Reset: time.Unix(1700000000, 0)},
	}
	for i := range want {
		got := rls[i]
		if got != want[i] {
			t.Fatalf("RateLimits()[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestRefreshRateLimitsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Client{
		hc:      &http.Client{Transport: http.DefaultTransport},
		ct:      &countingTransport{base: http.DefaultTransport},
		baseURL: srv.URL,
	}
	if err := c.RefreshRateLimits(context.Background()); err == nil {
		t.Fatal("RefreshRateLimits with HTTP 429 returned nil error")
	}
}

func TestFilterArchived(t *testing.T) {
	var repoHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoHits++
		switch r.URL.Path {
		case "/repos/org/archived":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"archived": true}`)
		case "/repos/org/live":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"archived": false}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{
		hc:       &http.Client{Transport: http.DefaultTransport},
		baseURL:  srv.URL,
		archived: map[string]bool{},
	}
	ctx := context.Background()
	prs := []PR{
		{Repo: "org/live", Number: 1},
		{Repo: "org/archived", Number: 2},
	}
	got := c.FilterArchived(ctx, prs)
	if len(got) != 1 || got[0].Repo != "org/live" || got[0].Number != 1 {
		t.Fatalf("FilterArchived = %+v, want only org/live#1", got)
	}

	// cached: a second pass makes no further /repos requests
	before := repoHits
	if got := c.FilterArchived(ctx, prs); len(got) != 1 {
		t.Fatalf("second FilterArchived = %+v, want only org/live#1", got)
	}
	if repoHits != before {
		t.Fatalf("/repos hits went from %d to %d on second pass; cache not used", before, repoHits)
	}
}

func TestFilterArchivedFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{
		hc:       &http.Client{Transport: http.DefaultTransport},
		baseURL:  srv.URL,
		archived: map[string]bool{},
	}
	prs := []PR{{Repo: "org/anywhere", Number: 7}}
	if got := c.FilterArchived(context.Background(), prs); len(got) != 1 {
		t.Fatalf("repo lookup failure should fail open and keep PRs, got %+v", got)
	}
}
