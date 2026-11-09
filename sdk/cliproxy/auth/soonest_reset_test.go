package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func routingTestAuth(id string) *Auth {
	return &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "test-token-" + id}, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}
}
func setRoutingTestQuota(s *SoonestResetSelector, a *Auth, observed time.Time, windows ...RoutingWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string]routingQuotaRecord)
	}
	s.records[a.ID] = routingQuotaRecord{fingerprint: quotaFingerprint(a), observedAt: observed, windows: windows}
}
func routingTestWindow(key string, now time.Time, reset time.Duration, used float64, seconds int64) RoutingWindow {
	return RoutingWindow{Key: key, ResetsAt: now.Add(reset), UsedPercent: used, seconds: seconds}
}

func TestSoonestResetRankingAndUnknowns(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*SoonestResetSelector, *Auth, *Auth)
		want   string
	}{
		{"weekly-before-short", nil, "b"},
		{"priority", func(s *SoonestResetSelector, a, b *Auth) { a.Attributes["priority"] = "4" }, "a"},
		{"missing", func(s *SoonestResetSelector, a, b *Auth) { delete(s.records, "b") }, "a"},
		{"stale", func(s *SoonestResetSelector, a, b *Auth) {
			r := s.records["b"]
			r.observedAt = now.Add(-16 * time.Minute)
			s.records["b"] = r
		}, "a"},
		{"expired", func(s *SoonestResetSelector, a, b *Auth) {
			r := s.records["b"]
			r.windows[1].ResetsAt = now
			s.records["b"] = r
		}, "a"},
		{"token-replaced", func(s *SoonestResetSelector, a, b *Auth) { b.Metadata["access_token"] = "new-token" }, "a"},
		{"tie", func(s *SoonestResetSelector, a, b *Auth) {
			r := s.records["b"]
			r.windows[1].ResetsAt = now.Add(48 * time.Hour)
			s.records["b"] = r
		}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &SoonestResetSelector{}
			a, b := routingTestAuth("a"), routingTestAuth("b")
			setRoutingTestQuota(s, a, now, routingTestWindow("short", now, time.Hour, 10, 18000), routingTestWindow("weekly", now, 48*time.Hour, 50, 604800))
			setRoutingTestQuota(s, b, now, routingTestWindow("short", now, 4*time.Hour, 10, 18000), routingTestWindow("weekly", now, 24*time.Hour, 50, 604800))
			if tc.mutate != nil {
				tc.mutate(s, a, b)
			}
			if got := s.rank([]*Auth{b, a}, "gpt-5.4", now)[0].ID; got != tc.want {
				t.Fatalf("rank=%s want=%s", got, tc.want)
			}
		})
	}
	s := &SoonestResetSelector{}
	a, b := routingTestAuth("a"), routingTestAuth("b")
	if got := s.rank([]*Auth{b, a}, "", now)[0].ID; got != "a" {
		t.Fatalf("missing fallback=%s", got)
	}
}

func TestRoutingQuotaParserContracts(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, provider, body string
		valid                bool
		count                int
	}{
		{"codex", "codex", fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":24,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":%d}},"code_review_rate_limit":{"primary_window":{"used_percent":100}}}`, now.Add(time.Hour).Unix(), now.Add(24*time.Hour).Unix()), true, 2},
		{"free-weekly-primary", "codex", `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":3600},"secondary_window":null}}`, true, 1},
		{"claude", "claude", `{"five_hour":{"utilization":0,"resets_at":"2026-11-01T01:00:00Z"},"seven_day":{"utilization":84,"resets_at":"2026-11-03T00:00:00Z"},"seven_day_opus":{"utilization":100,"resets_at":"2026-11-02T00:00:00Z"},"extra_usage":{"utilization":100}}`, true, 3},
		{"numeric-string", "codex", `{"rate_limit":{"primary_window":{"used_percent":"0","limit_window_seconds":604800,"reset_after_seconds":3600}}}`, false, 0},
		{"unknown-shape", "codex", `{"credits":{"balance":100}}`, false, 0},
		{"malformed", "claude", `{"seven_day":{"utilization":12,"resets_at":"invalid"}}`, false, 0},
		{"negative", "codex", `{"rate_limit":{"primary_window":{"used_percent":-1,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, valid := parseRoutingQuota(tc.provider, []byte(tc.body), now)
			if valid != tc.valid || len(w) != tc.count {
				t.Fatalf("windows=%+v valid=%v", w, valid)
			}
		})
	}
}

func TestSoonestResetExhaustionAndModelWindows(t *testing.T) {
	now := time.Now()
	s := &SoonestResetSelector{}
	a := routingTestAuth("a")
	a.Provider = "claude"
	short := routingTestWindow("five_hour", now, time.Hour, 100, 18000)
	week := routingTestWindow("seven_day", now, 24*time.Hour, 20, 604800)
	opus := routingTestWindow("seven_day_opus", now, 12*time.Hour, 100, 604800)
	opus.model = "opus"
	setRoutingTestQuota(s, a, now, short, week, opus)
	if until := s.blockedUntil(a, "claude-sonnet-4-6", now); !until.Equal(short.ResetsAt) {
		t.Fatalf("short limit=%v", until)
	}
	if until := s.blockedUntil(a, "claude-opus-4-6", now); !until.Equal(opus.ResetsAt) {
		t.Fatalf("opus limit=%v", until)
	}
	short.UsedPercent = 0
	setRoutingTestQuota(s, a, now, short, week, opus)
	if until := s.blockedUntil(a, "claude-sonnet-4-6", now); !until.IsZero() {
		t.Fatalf("unrelated opus quota blocks sonnet: %v", until)
	}
	_, window, _, _ := s.view(a, "claude-opus-4-6", now)
	if window.Key != "seven_day_opus" {
		t.Fatalf("window=%+v", window)
	}
}

type routingTestExecutor struct {
	provider  string
	http      func(context.Context, *Auth, *http.Request) (*http.Response, error)
	refreshes atomic.Int32
}

func (e *routingTestExecutor) Identifier() string { return e.provider }
func (e *routingTestExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *routingTestExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("unused")
}
func (e *routingTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *routingTestExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	e.refreshes.Add(1)
	return a, nil
}
func (e *routingTestExecutor) HttpRequest(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
	if e.http == nil {
		return nil, errors.New("unexpected probe")
	}
	return e.http(ctx, a, r)
}

func TestSoonestResetManagerAffinityPriorityAndPinnedContinuation(t *testing.T) {
	ctx := WithCodexClientRequest(context.Background(), true)
	now := time.Now()
	s := &SoonestResetSelector{}
	affinity := NewSessionAffinitySelector(s)
	defer affinity.Stop()
	m := NewManager(nil, affinity, nil)
	m.RegisterExecutor(&routingTestExecutor{provider: "codex"})
	model := "routing-regression-model"
	auths := make([]*Auth, 0, 2)
	for _, id := range []string{"a", "b"} {
		a, err := m.Register(ctx, routingTestAuth(t.Name()+id))
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, a)
		registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	a, b := auths[0], auths[1]
	setRoutingTestQuota(s, a, now, routingTestWindow("weekly", now, 24*time.Hour, 20, 604800))
	setRoutingTestQuota(s, b, now, routingTestWindow("weekly", now, 48*time.Hour, 20, 604800))
	pick := func(session string, extra map[string]any) *Auth {
		t.Helper()
		opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{session}}, Metadata: extra}
		got, err := m.SelectAuth(ctx, "codex", model, opts)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := pick("first", nil); got.ID != a.ID {
		t.Fatalf("first=%s", got.ID)
	}
	setRoutingTestQuota(s, b, now, routingTestWindow("weekly", now, 12*time.Hour, 20, 604800))
	if got := pick("first", nil); got.ID != a.ID {
		t.Fatalf("usable affinity moved=%s", got.ID)
	}
	if got := pick("new", nil); got.ID != b.ID {
		t.Fatalf("new session=%s", got.ID)
	}
	if got := pick("continuation", map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: a.ID}); got.ID != a.ID {
		t.Fatalf("pinned continuation moved=%s", got.ID)
	}
	setRoutingTestQuota(s, a, now, routingTestWindow("short", now, time.Hour, 100, 18000))
	if got := pick("first", nil); got.ID != b.ID {
		t.Fatalf("exhausted session did not fail over=%s", got.ID)
	}
	status := m.RoutingStatus(ctx, "codex", model, now)
	if status.NextAuthIndex != b.Index || !status.SessionAffinity {
		t.Fatalf("status=%+v", status)
	}
	doc := ojson.NewDocString(`{}`)
	if err := doc.Set("status", status); err != nil {
		t.Fatal(err)
	}
	payload := doc.Bytes()
	if strings.Contains(string(payload), "test-token-") || strings.Contains(string(payload), a.ID) {
		t.Fatal("status leaked credential material or ID")
	}
	setRoutingTestQuota(s, b, now, routingTestWindow("weekly", now, time.Hour, 100, 604800))
	if _, err := m.SelectAuth(ctx, "codex", model, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("all exhausted must fail")
	}
}

func TestRoutingQuotaProbeContractAndNoRefresh(t *testing.T) {
	m := NewManager(nil, &SoonestResetSelector{}, nil)
	exec := &routingTestExecutor{provider: "codex"}
	exec.http = func(ctx context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || r.Header.Get("Chatgpt-Account-Id") != "account" || IsCodexUseSIWCForRequest(ctx, a) {
			t.Error("wrong quota request contract")
		}
		if _, deadline := ctx.Deadline(); deadline {
			t.Error("probe set a request deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))}, nil
	}
	m.RegisterExecutor(exec)
	a := routingTestAuth("probe")
	a.Metadata["account_id"] = "account"
	a.Metadata["use_siwc"] = true
	windows, code := m.fetchRoutingQuota(context.Background(), a)
	if code != "" || len(windows) != 1 || exec.refreshes.Load() != 0 {
		t.Fatalf("probe=%+v code=%s refreshes=%d", windows, code, exec.refreshes.Load())
	}
}

func TestRoutingQuotaRefreshDiscoversAccountsAndRetainsLastObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &SoonestResetSelector{}
	m := NewManager(nil, s, nil)
	var calls atomic.Int32
	fail := atomic.Bool{}
	exec := &routingTestExecutor{provider: "codex", http: func(_ context.Context, a *Auth, r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if fail.Load() {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("private body"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":15,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))}, nil
	}}
	m.RegisterExecutor(exec)
	var auths []*Auth
	for i := 0; i < 9; i++ {
		a, err := m.Register(ctx, routingTestAuth(fmt.Sprintf("%s-%02d", t.Name(), i)))
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, a)
	}
	now := time.Now()
	// Explicit timestamps exercise more accounts than worker slots without sleeping.
	for i := 0; i < 3; i++ {
		m.refreshRoutingQuotas(ctx, now.Add(time.Duration(i)*30*time.Second)).Wait()
	}
	if calls.Load() != 9 {
		t.Fatalf("never-observed accounts starved, calls=%d", calls.Load())
	}
	for _, a := range auths {
		if r := s.record(a); r.observedAt.IsZero() {
			t.Fatalf("no observation for %s", a.ID)
		}
	}
	m.refreshRoutingQuotas(ctx, now.Add(time.Minute)).Wait()
	if calls.Load() != 9 {
		t.Fatal("polled account before refresh interval")
	}
	previous := s.record(auths[0])
	fail.Store(true)
	m.refreshRoutingQuotas(ctx, now.Add(6*time.Minute)).Wait()
	current := s.record(auths[0])
	if current.errorCode != "rate_limited" || !current.observedAt.Equal(previous.observedAt) || len(current.windows) != 1 {
		t.Fatalf("failed refresh erased last observation: %+v", current)
	}
	before := calls.Load()
	_ = m.RoutingStatus(ctx, "codex", "", now)
	if calls.Load() != before || exec.refreshes.Load() != 0 {
		t.Fatal("status read polled quota or refreshed credentials")
	}
}

func TestSoonestResetWebsocketPreferenceAndOneAccount(t *testing.T) {
	now := time.Now()
	s := &SoonestResetSelector{}
	a, b := routingTestAuth("a"), routingTestAuth("b")
	b.Attributes["websockets"] = "true"
	setRoutingTestQuota(s, a, now, routingTestWindow("weekly", now, time.Hour, 10, 604800))
	setRoutingTestQuota(s, b, now, routingTestWindow("weekly", now, 24*time.Hour, 10, 604800))
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	got, err := s.Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if err != nil || got.ID != b.ID {
		t.Fatalf("WS preference got=%v err=%v", got, err)
	}
	got, err = s.Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{a})
	if err != nil || got.ID != a.ID {
		t.Fatalf("one account got=%v err=%v", got, err)
	}
	b.Disabled = true
	got, err = s.Pick(ctx, "codex", "model", cliproxyexecutor.Options{}, []*Auth{a, b})
	if err != nil || got.ID != a.ID {
		t.Fatalf("disabled WS account got=%v err=%v", got, err)
	}
}

func TestSoonestResetResolvedModelAndPriorityFailover(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := &SoonestResetSelector{}
	m := NewManager(nil, s, nil)
	m.RegisterExecutor(&routingTestExecutor{provider: "claude"})
	const alias = "routing-test-alias"
	const target = "claude-opus-4-6"
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: target, Alias: alias, Fork: true}}})
	var auths []*Auth
	for i, id := range []string{t.Name() + "a", t.Name() + "b"} {
		a := routingTestAuth(id)
		a.Provider = "claude"
		if i == 0 {
			a.Attributes["priority"] = "10"
		}
		saved, err := m.Register(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, saved)
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: alias}, {ID: target}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	a, b := auths[0], auths[1]
	global := routingTestWindow("seven_day", now, 24*time.Hour, 10, 604800)
	opus := routingTestWindow("seven_day_opus", now, 12*time.Hour, 100, 604800)
	opus.model = "opus"
	setRoutingTestQuota(s, a, now, global, opus)
	setRoutingTestQuota(s, b, now, global)
	got, err := m.SelectAuth(ctx, "claude", alias, cliproxyexecutor.Options{})
	if err != nil || got.ID != b.ID {
		t.Fatalf("model quota should skip exhausted higher priority: got=%v err=%v", got, err)
	}
	if status := m.RoutingStatus(ctx, "claude", alias, now); status.NextAuthIndex != b.Index {
		t.Fatalf("status differs from selection: %+v", status)
	}
	opus.UsedPercent = 20
	setRoutingTestQuota(s, a, now, global, opus)
	got, err = m.SelectAuth(ctx, "claude", alias, cliproxyexecutor.Options{})
	if err != nil || got.ID != a.ID {
		t.Fatalf("usable higher priority ignored: got=%v err=%v", got, err)
	}
}

func TestRoutingQuotaRefreshDiscardsReplacedCredential(t *testing.T) {
	ctx := context.Background()
	s := &SoonestResetSelector{}
	m := NewManager(nil, s, nil)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m.RegisterExecutor(&routingTestExecutor{provider: "codex", http: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"primary_window":{"used_percent":90,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))}, nil
	}})
	a, err := m.Register(ctx, routingTestAuth(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	pending := m.refreshRoutingQuotas(ctx, time.Now())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe never started")
	}
	replacement := a.Clone()
	replacement.Metadata["access_token"] = "replacement-token"
	replacement, err = m.Update(ctx, replacement)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	pending.Wait()
	if r := s.record(replacement); !r.observedAt.IsZero() {
		t.Fatal("old-token observation attached to replacement")
	}
	m.refreshRoutingQuotas(ctx, time.Now()).Wait()
	if r := s.record(replacement); r.observedAt.IsZero() {
		t.Fatal("replacement did not get its own observation")
	}
}

func TestSoonestResetCodexNamedPoolDoesNotUseDefaultQuota(t *testing.T) {
	now := time.Now()
	s := &SoonestResetSelector{}
	a := routingTestAuth("pool")
	main := routingTestWindow("secondary_window", now, 24*time.Hour, 100, 604800)
	spark := routingTestWindow("additional:gpt53codexspark:primary_window", now, time.Hour, 10, 18000)
	spark.model = "gpt53codexspark"
	setRoutingTestQuota(s, a, now, main, spark)
	if until := s.blockedUntil(a, "gpt-5.3-codex-spark", now); !until.IsZero() {
		t.Fatalf("default pool blocked independent named pool: %v", until)
	}
	if until := s.blockedUntil(a, "gpt-5.4", now); until.IsZero() {
		t.Fatal("ordinary request ignored main quota")
	}
	_, w, _, _ := s.view(a, "gpt-5.3-codex-spark", now)
	if w == nil || w.Key != spark.Key {
		t.Fatalf("wrong active quota window %+v", w)
	}
}
