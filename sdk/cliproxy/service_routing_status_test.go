package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestServiceRunRoutingStatusWithoutPluginScheduler(t *testing.T) {
	for _, strategy := range []string{"fill-first", "soonest-reset-first"} {
		t.Run(strategy, func(t *testing.T) {
			dir := t.TempDir()
			catalogDir, errAbs := filepath.Abs(filepath.Join("..", "..", "internal", "registry", "models"))
			if errAbs != nil {
				t.Fatal(errAbs)
			}
			raw := fmt.Sprintf("host: 127.0.0.1\nport: 0\nauth-dir: %q\nplugins:\n  enabled: false\nrouting:\n  strategy: %s\n  session-affinity: true\nmodels:\n  catalog: %q\n  codex-catalog: %q\n  devin-catalog: %q\nremote-management:\n  secret-key: synthetic-routing-test\n  disable-control-panel: true\n", filepath.Join(dir, "auths"), strategy, filepath.Join(catalogDir, "models.json"), filepath.Join(catalogDir, "codex_client_models.json"), filepath.Join(catalogDir, "devin_models.json"))
			cfg, errParse := internalconfig.ParseConfigBytes([]byte(raw))
			if errParse != nil {
				t.Fatal(errParse)
			}
			configPath := filepath.Join(dir, "config.yaml")
			if errWrite := os.WriteFile(configPath, []byte(raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			ready := make(chan http.Handler, 1)
			release := make(chan struct{})
			releaseStartup := sync.OnceFunc(func() { close(release) })
			service, errBuild := NewBuilder().WithConfig(cfg).WithConfigPath(configPath).
				WithHooks(Hooks{OnAfterStart: func(s *Service) {
					// Run has wired and synchronized the real plugin host and API server.
					// Hold startup here so the watcher cannot replace the synthetic model.
					ready <- s.server.Handler()
					<-release
				}}).Build()
			if errBuild != nil {
				t.Fatal(errBuild)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- service.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				releaseStartup()
				select {
				case errRun := <-done:
					if errRun != nil && !errors.Is(errRun, context.Canceled) {
						t.Errorf("service.Run: %v", errRun)
					}
				case <-time.After(10 * time.Second):
					t.Error("service did not stop")
				}
			})
			var handler http.Handler
			select {
			case handler = <-ready:
			case <-time.After(10 * time.Second):
				t.Fatal("service did not start")
			}
			if service.pluginHost == nil || service.pluginHost.HasScheduler() {
				t.Fatal("expected real plugin host without an active scheduler")
			}
			requestStatus := func(wantFallback, wantNext string) {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, "/v8/management/routing/status?provider=codex&model=routing-service-test&transport=websocket", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", "Bearer synthetic-routing-test")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				body := ojson.Parse(rec.Body.String())
				if rec.Code != http.StatusOK || body.Get("fallback_reason").String() != wantFallback || body.Get("next_auth_index").String() != wantNext || body.Get("strategy").String() != strategy || !body.Get("session_affinity").Bool() || body.Get("transport").String() != "websocket" {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
			if strategy == "fill-first" {
				requestStatus("strategy_not_predictable", "")
				return
			}
			requestStatus("no_eligible_accounts", "")
			a, errRegister := service.coreManager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: "routing-service-test"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
			requestStatus("quota_unavailable", a.Index)
		})
	}
}
