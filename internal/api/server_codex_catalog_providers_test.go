package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/json"
	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestCodexClientModelsEndpoint_CatalogProviders(t *testing.T) {
	codexModel := "catalog-providers-endpoint-codex"
	claudeModel := "catalog-providers-endpoint-claude"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("catalog-providers-endpoint-codex-client", "codex", []*registry.ModelInfo{{ID: codexModel, Object: "model", OwnedBy: "openai"}})
	modelRegistry.RegisterClient("catalog-providers-endpoint-claude-client", "claude", []*registry.ModelInfo{{ID: claudeModel, Object: "model", OwnedBy: "anthropic"}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("catalog-providers-endpoint-codex-client")
		modelRegistry.UnregisterClient("catalog-providers-endpoint-claude-client")
	})

	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.Client.Codex.CatalogProviders = "codex"
	server := newTestServerWithConfig(t, cfg)

	get := func(path string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}

	var catalog struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(get("/v1/models?client_version=0.172.0"), &catalog); err != nil {
		t.Fatalf("parse Codex catalog: %v", err)
	}
	slugs := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		slugs[model.Slug] = true
	}
	if !slugs[codexModel] || slugs[claudeModel] {
		t.Fatalf("Codex catalog = %v, want %q without %q", slugs, codexModel, claudeModel)
	}

	// Clients that do not send client_version keep the full list.
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(get("/v1/models"), &list); err != nil {
		t.Fatalf("parse OpenAI model list: %v", err)
	}
	listed := false
	for _, model := range list.Data {
		if model.ID == claudeModel {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("/v1/models no longer lists %q", claudeModel)
	}
}

func TestHomeCodexClientModels_CatalogProviders(t *testing.T) {
	entries := []homeModelEntry{
		{id: "home-codex", providers: []string{"codex"}},
		{id: "home-claude", providers: []string{"claude"}},
	}
	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, formatHomeCodexModel(entry))
	}
	filtered := codexmodels.FilterByProviders(models, "codex", homeProvidersForModel(entries))
	if len(filtered) != 1 || filtered[0]["id"] != "home-codex" {
		t.Fatalf("filtered Home models = %v, want only home-codex", filtered)
	}
}
