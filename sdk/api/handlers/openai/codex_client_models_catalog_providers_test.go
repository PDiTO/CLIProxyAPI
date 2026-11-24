package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestCodexClientModelsResponseCatalogProviders(t *testing.T) {
	codexModel := "catalog-providers-test-codex"
	claudeModel := "catalog-providers-test-claude"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("catalog-providers-test-codex-client", "codex", []*registry.ModelInfo{{ID: codexModel}})
	modelRegistry.RegisterClient("catalog-providers-test-claude-client", "claude", []*registry.ModelInfo{{ID: claudeModel}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("catalog-providers-test-codex-client")
		modelRegistry.UnregisterClient("catalog-providers-test-claude-client")
	})

	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	handler := NewOpenAIAPIHandler(base)
	slugs := func() map[string]bool {
		response := handler.codexClientModelsResponse("0.172.0")
		models, ok := response["models"].([]map[string]any)
		if !ok {
			t.Fatalf("models type = %T, want []map[string]any", response["models"])
		}
		found := make(map[string]bool, len(models))
		for _, model := range models {
			slug, _ := model["slug"].(string)
			found[slug] = true
		}
		return found
	}

	if found := slugs(); !found[codexModel] || !found[claudeModel] {
		t.Fatalf("default catalog = %v, want both test models", found)
	}

	base.Cfg.Client.Codex.CatalogProviders = "codex"
	found := slugs()
	if !found[codexModel] {
		t.Fatalf("filtered catalog is missing %q", codexModel)
	}
	if found[claudeModel] {
		t.Fatalf("filtered catalog still lists %q", claudeModel)
	}

	// The plain OpenAI model list used by other clients is not filtered.
	listed := false
	for _, model := range handler.Models() {
		if id, _ := model["id"].(string); id == claudeModel {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("Models() no longer lists %q", claudeModel)
	}
}
