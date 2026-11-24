package cliproxy

import (
	"context"
	"testing"
	"time"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Config reloads apply Codex model overrides to registered Codex OAuth auths.
func TestServiceConfigReloadAppliesCodexModelOverrides(t *testing.T) {
	const authID = "codex-override-reload-oauth"
	const overrideID = "gpt-6.3-sol"
	modelRegistry := internalregistry.GetGlobalRegistry()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID: authID, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "pro"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	s := &Service{cfg: &config.Config{}, coreManager: manager}
	s.registerModelRefreshCallback()
	t.Cleanup(func() {
		internalregistry.SetModelRefreshCallback(nil)
		internalregistry.ApplyCodexModelOverrides(nil, false)
		modelRegistry.UnregisterClient(authID)
	})
	s.registerModelsForAuth(context.Background(), auth)

	hasOverride := func() bool {
		for _, model := range modelRegistry.GetModelsForClient(authID) {
			if model.ID == overrideID {
				return true
			}
		}
		return false
	}
	await := func(want bool) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for hasOverride() != want {
			select {
			case <-ticker.C:
			case <-deadline.C:
				t.Fatalf("override registered = %v, want %v", !want, want)
			}
		}
	}
	if hasOverride() {
		t.Fatal("override model registered before configuration")
	}

	updated := &config.Config{}
	updated.Models.CodexOverrides = []internalregistry.CodexModelOverride{{ID: overrideID, Like: "gpt-6.2-sol"}}
	if commit := s.commitConfigUpdate(updated); commit.cfg == nil {
		t.Fatal("config update rejected")
	}
	await(true)
	if providers := modelRegistry.GetModelProviders(overrideID); len(providers) != 1 || providers[0] != "codex" {
		t.Fatalf("providers = %v, want [codex]", providers)
	}

	if commit := s.commitConfigUpdate(&config.Config{}); commit.cfg == nil {
		t.Fatal("config update rejected")
	}
	await(false)

	invalid := &config.Config{}
	invalid.Models.CodexOverrides = []internalregistry.CodexModelOverride{{ID: overrideID}}
	if commit := s.commitConfigUpdate(invalid); commit.cfg != nil {
		t.Fatal("invalid overrides were accepted")
	}
}
