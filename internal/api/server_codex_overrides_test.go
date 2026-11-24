package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestCodexModelOverrideServedInCodexCatalog(t *testing.T) {
	registry.ApplyCodexModelOverrides([]registry.CodexModelOverride{
		{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol"},
		{ID: "gpt-6.2-astra", AddServiceTiers: []string{"ultrafast"}},
	}, false)
	const clientID = "codex-override-catalog-client"
	modelRegistry := registry.GetGlobalRegistry()
	// Codex OAuth auths register the plan list, which now includes the clone.
	modelRegistry.RegisterClient(clientID, "codex", registry.GetCodexProModels())
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
		registry.ApplyCodexModelOverrides(nil, false)
	})
	if providers := modelRegistry.GetModelProviders("gpt-6.3-sol"); len(providers) != 1 || providers[0] != "codex" {
		t.Fatalf("gpt-6.3-sol providers = %v, want [codex]", providers)
	}

	server := newTestServerWithConfig(t, &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.172.0", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	entries := map[string]ojson.Result{}
	for _, model := range ojson.Get(rr.Body.String(), "models").Array() {
		entries[model.Get("slug").String()] = model
	}
	clone, like := entries["gpt-6.3-sol"], entries["gpt-6.2-sol"]
	if !clone.Exists() || !like.Exists() {
		t.Fatalf("catalog slugs missing: clone=%v like=%v", clone.Exists(), like.Exists())
	}
	// The Codex catalog shows the registry display name, as for upstream models.
	if clone.Get("display_name").String() != "GPT 6.3 Sol" || clone.Get("base_instructions").String() != like.Get("base_instructions").String() {
		t.Fatalf("clone entry does not use the like template: %s", clone.Get("display_name").Raw)
	}
	if clone.Get("service_tiers").Raw != like.Get("service_tiers").Raw {
		t.Fatal("clone entry lost the like model's service tiers")
	}
	astraTiers := entries["gpt-6.2-astra"].Get("service_tiers.#.id").Raw
	if !strings.Contains(astraTiers, "ultrafast") {
		t.Fatalf("gpt-6.2-astra service tiers = %s, want ultrafast", astraTiers)
	}
}

func TestCodexModelOverridesManagementEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("config-version: 8\nremote-management:\n  secret-key: test-password\n")
	if errWrite := os.WriteFile(path, raw, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := proxyconfig.LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	h := management.NewHandler(cfg, path, coreauth.NewManager(nil, nil, nil))
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	t.Cleanup(func() { registry.ApplyCodexModelOverrides(nil, false) })

	call := func(method, body string, authorized bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v8/management/codex-model-overrides", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		return rec
	}

	if rec := call(http.MethodGet, "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d", rec.Code)
	}
	if rec := call(http.MethodGet, "", true); rec.Code != http.StatusOK || rec.Body.String() != `{"overrides":[]}` {
		t.Fatalf("empty GET = %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{}`, `not json`, `{"overrides":[{"id":"gpt-6.3-sol"}]}`, `{"overrides":[{"id":"a","like":"a"}]}`} {
		if rec := call(http.MethodPut, body, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s status = %d", body, rec.Code)
		}
	}

	put := `{"overrides":[{"id":"gpt-6.3-sol","like":"gpt-6.2-sol","display-name":"GPT 6.3 Sol"},{"id":"gpt-6.2-sol","add-service-tiers":["ultrafast"]},{"id":"gpt-7","like":"gpt-missing"}]}`
	rec := call(http.MethodPut, put, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	wantStates := map[string]string{
		"gpt-6.3-sol": `{"model":"active","catalog":"active","service-tiers":{}}`,
		"gpt-6.2-sol": `{"model":"none","catalog":"none","service-tiers":{"ultrafast":"upstream"}}`,
		"gpt-7":       `{"model":"inactive","catalog":"inactive","service-tiers":{}}`,
	}
	checkStates := func(body string) {
		t.Helper()
		overrides := ojson.Get(body, "overrides").Array()
		if len(overrides) != len(wantStates) {
			t.Fatalf("overrides = %s", body)
		}
		for _, override := range overrides {
			if got := override.Get("state").Raw; got != wantStates[override.Get("id").String()] {
				t.Fatalf("state for %s = %s", override.Get("id").String(), got)
			}
			if !override.Get("add-service-tiers").IsArray() {
				t.Fatalf("add-service-tiers missing: %s", override.Raw)
			}
		}
	}
	checkStates(rec.Body.String())
	if got := ojson.Get(rec.Body.String(), "overrides.0.display-name").String(); got != "GPT 6.3 Sol" {
		t.Fatalf("display-name = %q", got)
	}

	rec = call(http.MethodGet, "", true)
	checkStates(rec.Body.String())
	if _, count := codexOverrideTestCount(registry.GetCodexProModels(), "gpt-6.3-sol"); count != 1 {
		t.Fatal("PUT did not apply the override live")
	}

	saved, errReload := proxyconfig.LoadConfig(path)
	if errReload != nil {
		t.Fatal(errReload)
	}
	if len(saved.Models.CodexOverrides) != 3 || saved.Models.CodexOverrides[1].AddServiceTiers[0] != "ultrafast" || saved.Models.CodexOverrides[0].DisplayName != "GPT 6.3 Sol" {
		t.Fatalf("persisted overrides = %+v", saved.Models.CodexOverrides)
	}
	if saved.RemoteManagement.SecretKey == "" {
		t.Fatal("saving overrides dropped other settings")
	}

	if rec = call(http.MethodPut, `{"overrides":[]}`, true); rec.Code != http.StatusOK || rec.Body.String() != `{"overrides":[]}` {
		t.Fatalf("clearing PUT = %d %s", rec.Code, rec.Body.String())
	}
	if _, count := codexOverrideTestCount(registry.GetCodexProModels(), "gpt-6.3-sol"); count != 0 {
		t.Fatal("clearing overrides left the model registered")
	}
	cleared, errCleared := proxyconfig.LoadConfig(path)
	if errCleared != nil {
		t.Fatal(errCleared)
	}
	if len(cleared.Models.CodexOverrides) != 0 {
		t.Fatalf("cleared overrides persisted as %+v", cleared.Models.CodexOverrides)
	}
}

func codexOverrideTestCount(models []*registry.ModelInfo, id string) (*registry.ModelInfo, int) {
	var found *registry.ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == id {
			found = model
			count++
		}
	}
	return found, count
}
