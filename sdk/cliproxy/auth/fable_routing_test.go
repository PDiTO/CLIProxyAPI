package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRoutingQuotaClaudeFableFormats(t *testing.T) {
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		valid      bool
		count      int
		used       float64
	}{
		{"scoped", `{"limits":[{"kind":"weekly_scoped","percent":64,"resets_at":"2026-11-11T00:00:00Z","is_active":false,"scope":{"model":{"id":null,"display_name":"Fable"}}}]}`, true, 1, 64},
		{"normalized-family", `{"limits":[{"kind":" Weekly_Scoped ","percent":32,"resets_at":"2026-11-11T00:00:00Z","scope":{"model":{"display_name":" FABLE 5 "}}}]}`, true, 1, 32},
		{"legacy", `{"iguana_necktie":{"utilization":41,"resets_at":"2026-11-11T00:00:00Z"}}`, true, 1, 41},
		{"active-scoped-preferred", `{"iguana_necktie":{"utilization":100,"resets_at":"2026-11-11T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":100,"resets_at":"2026-11-11T00:00:00Z","scope":{"model":{"display_name":"Fable 5"}}},{"kind":"weekly_scoped","percent":12,"resets_at":"2026-11-11T00:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable"}}}]}`, true, 1, 12},
		{"idle-scoped-clears-legacy", `{"iguana_necktie":{"utilization":100,"resets_at":"2026-11-11T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":0,"resets_at":null,"scope":{"model":{"display_name":"Fable"}}}]}`, true, 0, 0},
		{"over-limit", `{"limits":[{"kind":"weekly_scoped","percent":100.4,"resets_at":"2026-11-11T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`, true, 1, 100.4},
		{"idle-legacy", `{"iguana_necktie":{"utilization":0,"resets_at":null}}`, true, 0, 0},
		{"dollar-balance", `{"iguana_necktie":{"utilization":100,"resets_at":"2026-11-11T00:00:00Z","remaining_dollars":0},"seven_day":{"utilization":20,"resets_at":"2026-11-12T00:00:00Z"}}`, true, 1, 20},
		{"null-dollar-fields", `{"iguana_necktie":{"utilization":41,"resets_at":"2026-11-11T00:00:00Z","limit_dollars":null,"used_dollars":null,"remaining_dollars":null}}`, true, 1, 41},
		{"ignore-unrelated", `{"seven_day":{"utilization":20,"resets_at":"2026-11-12T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Sonnet"}}},{"kind":"session","percent":100,"scope":{"model":{"display_name":"Fable"}}}]}`, true, 1, 20},
		{"bad-percent", `{"seven_day":{"utilization":20,"resets_at":"2026-11-12T00:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":"100","resets_at":"2026-11-11T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}]}`, false, 1, 20},
		{"missing-reset", `{"limits":[{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Fable"}}}]}`, false, 0, 0},
		{"used-without-reset", `{"limits":[{"kind":"weekly_scoped","percent":12,"resets_at":null,"scope":{"model":{"display_name":"Fable"}}}]}`, false, 0, 0},
		{"malformed-limits", `{"seven_day":{"utilization":20,"resets_at":"2026-11-12T00:00:00Z"},"limits":{}}`, false, 1, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			windows, valid := parseRoutingQuota("claude", []byte(tc.body), now)
			if valid != tc.valid || len(windows) != tc.count {
				t.Fatalf("windows=%+v valid=%v", windows, valid)
			}
			if len(windows) > 0 && windows[0].UsedPercent != tc.used {
				t.Fatalf("usage=%v want=%v", windows[0].UsedPercent, tc.used)
			}
		})
	}
}

func fableRoutingQuota(t *testing.T, now time.Time, sharedReset, fableReset time.Duration, sharedUsed, fableUsed float64) []RoutingWindow {
	t.Helper()
	body := fmt.Sprintf(`{"five_hour":null,"seven_day":{"utilization":%g,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":%g,"resets_at":%q,"is_active":true,"scope":{"model":{"id":null,"display_name":"Fable"}}}]}`, sharedUsed, now.Add(sharedReset).Format(time.RFC3339Nano), fableUsed, now.Add(fableReset).Format(time.RFC3339Nano))
	windows, valid := parseRoutingQuota("claude", []byte(body), now)
	if !valid {
		t.Fatal("invalid quota fixture")
	}
	return windows
}

func TestSoonestResetFableSharedLimitsRankingAndFailover(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	s := &SoonestResetSelector{}
	affinity := NewSessionAffinitySelector(s)
	defer affinity.Stop()
	m := NewManager(nil, affinity, nil)
	m.RegisterExecutor(&routingTestExecutor{provider: "claude"})
	const alias = "fable-routing-alias"
	const target = "claude-fable-5-2"
	m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: target, Alias: alias, Fork: true}}})
	models := []string{"claude-fable-5", "claude-fable-5-1", target, alias, "claude-sonnet-4-6"}
	var auths []*Auth
	for _, suffix := range []string{"a", "b"} {
		a := routingTestAuth(t.Name() + suffix)
		a.Provider = "claude"
		saved, err := m.Register(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		auths = append(auths, saved)
		var infos []*registry.ModelInfo
		for _, model := range models {
			infos = append(infos, &registry.ModelInfo{ID: model})
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", infos)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	}
	a, b := auths[0], auths[1]
	setA := func(weekly, fable float64) {
		setRoutingTestQuota(s, a, now, fableRoutingQuota(t, now, 72*time.Hour, 12*time.Hour, weekly, fable)...)
	}
	setA(20, 50)
	setRoutingTestQuota(s, b, now, fableRoutingQuota(t, now, 24*time.Hour, 48*time.Hour, 20, 50)...)
	pick := func(model, session string, want *Auth) {
		t.Helper()
		opts := cliproxyexecutor.Options{Headers: map[string][]string{"Session_id": {session}}}
		got, err := m.SelectAuth(ctx, "claude", model, opts)
		if err != nil || got.ID != want.ID {
			t.Fatalf("model=%s got=%v err=%v want=%s", model, got, err, want.ID)
		}
	}
	for _, model := range models[:4] {
		pick(model, "", a)
		status := m.RoutingStatus(ctx, "claude", model, now)
		if status.NextAuthIndex != a.Index || status.Accounts[0].QuotaState != "fresh" {
			t.Fatalf("Fable status disagrees with routing: %+v", status)
		}
	}
	pick("claude-sonnet-4-6", "", b)
	pick(target, "fable-session", a)
	setRoutingTestQuota(s, b, now, fableRoutingQuota(t, now, 24*time.Hour, 6*time.Hour, 20, 50)...)
	pick(target, "fable-session", a)
	pick(target, "new-fable-session", b)
	setA(20, 100.4)
	pick(target, "fable-session", b)
	status := m.RoutingStatus(ctx, "claude", alias, now)
	if status.Accounts[0].Eligible || status.Accounts[0].SkippedReason != "quota_exhausted" || status.NextAuthIndex != b.Index {
		t.Fatalf("exhausted Fable account remains eligible: %+v", status)
	}
	if until := s.blockedUntil(a, "claude-sonnet-4-6", now); !until.IsZero() {
		t.Fatalf("Fable exhaustion blocked Sonnet until %v", until)
	}
	setA(100, 20)
	pick(alias, "", b)
	if s.blockedUntil(a, target, now).IsZero() {
		t.Fatal("Fable allowance replaced shared weekly limit")
	}
	setRoutingTestQuota(s, b, now, fableRoutingQuota(t, now, 24*time.Hour, 6*time.Hour, 20, 100)...)
	if _, err := m.SelectAuth(ctx, "claude", target, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("all exhausted accounts must refuse Fable")
	}
	setA(20, 0)
	pick(target, "after-quota-refresh", a)
	short := routingTestWindow("five_hour", now, time.Hour, 100, 18000)
	windows := append(fableRoutingQuota(t, now, 72*time.Hour, 12*time.Hour, 20, 0), short)
	setRoutingTestQuota(s, a, now, windows...)
	if until := s.blockedUntil(a, target, now); !until.Equal(short.ResetsAt) {
		t.Fatalf("Fable ignored exhausted short window: %v", until)
	}
}

func TestRoutingQuotaFableRefreshKeepsCompleteSnapshot(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	s := &SoonestResetSelector{}
	m := NewManager(nil, s, nil)
	body := func(percent string) string {
		return fmt.Sprintf(`{"seven_day":{"utilization":20,"resets_at":%q},"limits":[{"kind":"weekly_scoped","percent":%s,"resets_at":%q,"scope":{"model":{"display_name":"Fable"}}}]}`, now.Add(24*time.Hour).Format(time.RFC3339Nano), percent, now.Add(24*time.Hour).Format(time.RFC3339Nano))
	}
	response := body("100")
	m.RegisterExecutor(&routingTestExecutor{provider: "claude", http: func(context.Context, *Auth, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	}})
	a := routingTestAuth(t.Name())
	a.Provider = "claude"
	a, err := m.Register(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	m.refreshRoutingQuotas(ctx, now).Wait()
	if s.blockedUntil(a, "claude-fable-5-2", time.Now()).IsZero() {
		t.Fatal("initial Fable exhaustion was not observed")
	}
	previous := s.record(a)
	response = body(`"bad"`)
	m.refreshRoutingQuotas(ctx, now.Add(6*time.Minute)).Wait()
	current := s.record(a)
	if current.errorCode != "malformed" || !current.observedAt.Equal(previous.observedAt) || s.blockedUntil(a, "claude-fable-5-2", time.Now()).IsZero() {
		t.Fatal("malformed Fable response replaced the previous complete snapshot")
	}
	response = body("0")
	m.refreshRoutingQuotas(ctx, now.Add(12*time.Minute)).Wait()
	if current := s.record(a); current.errorCode != "" || !s.blockedUntil(a, "claude-fable-5-2", time.Now()).IsZero() {
		t.Fatal("fresh restored Fable quota remained blocked")
	}
}
