package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nonlinear-xyz/shale/internal/pack"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func sample() []pack.Evidence {
	return []pack.Evidence{{Ref: "memory:a@1", Title: "Signing", Content: "Use the release certificate", Prov: pack.Provenance{Epistemic: "asserted"}}, {Ref: "chunk:2:0", Title: "/private/source/path", Content: "Previous signing failed"}}
}
func response(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func valid() string {
	return `{"model":"jev-1.13.0","answers":{"c0":{"type":"score","score":3,"confidence":0.9},"c1":{"type":"score","score":1,"confidence":0.8}},"usage":{"input_tokens":123,"output_tokens":12}}`
}

func TestRankBatchAndSafePayload(t *testing.T) {
	calls := 0
	c := New("test-key", DefaultModel)
	c.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != Endpoint || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("wrong endpoint/auth")
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "memory:a") || strings.Contains(string(b), "/private/source/path") || strings.Contains(string(b), "super-secret-value") {
			t.Fatal("leaked metadata or secret")
		}
		var req request
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Questions) != 2 || req.State.Task == "" {
			t.Fatal("not batched")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("no deadline")
		}
		return response(valid(), 200), nil
	})}
	scores, report, err := c.Rank(context.Background(), "sign release SECRET=super-secret-value", sample())
	if err != nil || calls != 1 || scores["memory:a@1"].Score != 3 || report.InputTokens != 123 {
		t.Fatalf("%v %+v %+v", err, scores, report)
	}
}
func TestRejectResponsesAndNeverEchoBody(t *testing.T) {
	for _, body := range []string{`{`, strings.Replace(valid(), `"confidence":0.9`, `"confidence":2`, 1), strings.Replace(valid(), `"score":3`, `"score":4`, 1), strings.Replace(valid(), `"score":3,`, "", 1), strings.Replace(valid(), `"c1":`, `"unknown":`, 1), valid() + ` trailing`} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			c := New("key", "")
			c.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) { return response(body, 200), nil })}
			_, r, e := c.Rank(context.Background(), "task", sample())
			if e == nil || r.Reason != "invalid_response" {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
	c := New("key", "")
	calls := 0
	c.HTTP = &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		calls++
		return response("sensitive provider body", 429), nil
	})}
	_, r, e := c.Rank(context.Background(), "task", sample())
	if calls != 1 || e.Error() != "http_429" || r.Reason != "http_429" {
		t.Fatal(r, e)
	}
}
func TestTimeoutAndCancellation(t *testing.T) {
	c := New("key", "")
	c.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	start := time.Now()
	_, r, e := c.Rank(context.Background(), "task", sample())
	if e == nil || r.Reason != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatal(r, e, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, e = c.Rank(ctx, "task", sample())
	if e != context.Canceled {
		t.Fatal(e)
	}
}
func TestPayloadBoundsAndUTF8(t *testing.T) {
	es := sample()
	for i := range es {
		es[i].Content = strings.Repeat("界\n\"", 10000)
	}
	p, e := Prepare("task", es, DefaultModel)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p.body)
	s, _ := json.Marshal(p.body.State)
	if len(b) > MaxRequestBytes || len(s) > MaxStateBytes || !p.body.State.Candidates[0].Excerpted {
		t.Fatal("unbounded payload")
	}
	_, e = Prepare(strings.Repeat("x", MaxStateBytes+1), es, DefaultModel)
	if e == nil {
		t.Fatal("oversize task accepted")
	}
}
func TestExplicitOptIn(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "key")
	t.Setenv("SHALE_CONTEXT_RERANKER", "")
	r, e := FromEnv()
	if r != nil || e != nil {
		t.Fatal("key enabled Jev")
	}
	t.Setenv("SHALE_CONTEXT_RERANKER", "unknown")
	if _, e = FromEnv(); e == nil {
		t.Fatal("unknown accepted")
	}
	t.Setenv("SHALE_CONTEXT_RERANKER", "jev")
	t.Setenv("TYPESAFE_API_KEY", "")
	if _, e = FromEnv(); e == nil {
		t.Fatal("missing key accepted")
	}
	t.Setenv("TYPESAFE_API_KEY", "key")
	if r, e = FromEnv(); e != nil || r == nil {
		t.Fatal(e)
	}
}
func TestSubsetPreservesState(t *testing.T) {
	c := New("key", "")
	p, _ := Prepare("task", sample(), c.Model)
	original, _ := json.Marshal(p.body.State)
	c.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		var q request
		json.NewDecoder(r.Body).Decode(&q)
		s, _ := json.Marshal(q.State)
		if string(s) != string(original) || len(q.Questions) != 1 {
			t.Fatal("state changed")
		}
		return response(`{"model":"jev-1.13.0","answers":{"c1":{"type":"score","score":2,"confidence":1}}}`, 200), nil
	})}
	if _, _, e := c.Evaluate(context.Background(), p, []string{"c1"}); e != nil {
		t.Fatal(e)
	}
	if len(p.body.Questions) != 2 {
		t.Fatal("mutated prepared request")
	}
}
func BenchmarkPrepare(b *testing.B) {
	es := sample()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Prepare("sign release", es, DefaultModel); err != nil {
			b.Fatal(err)
		}
	}
}
