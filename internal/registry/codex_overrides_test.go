package registry

import (
	"bytes"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// useCodexOverrideTestCatalogs restores the embedded catalogs and clears
// overrides after the test.
func useCodexOverrideTestCatalogs(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ApplyCodexModelOverrides(nil, false)
		if errLoad := loadModelsFromBytes(embeddedModelsJSON, "embed"); errLoad != nil {
			t.Errorf("restore models catalog: %v", errLoad)
		}
		if _, errLoad := loadCodexClientModelsFromBytes(embeddedCodexClientModelsJSON, "embed"); errLoad != nil {
			t.Errorf("restore Codex client catalog: %v", errLoad)
		}
	})
}

func codexClientModelEntry(t *testing.T, slug string) (ojson.Result, int) {
	t.Helper()
	var found ojson.Result
	count := 0
	for _, model := range ojson.GetBytes(GetCodexClientModelsJSON(), "models").Array() {
		if model.Get("slug").String() == slug {
			found = model
			count++
		}
	}
	return found, count
}

func countModels(models []*ModelInfo, id string) (*ModelInfo, int) {
	var found *ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == id {
			found = model
			count++
		}
	}
	return found, count
}

func TestCodexModelOverrideClonesLikeModel(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	ApplyCodexModelOverrides([]CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol"}}, false)

	like, _ := countModels(GetCodexProModels(), "gpt-6.2-sol")
	clone, count := countModels(GetCodexProModels(), "gpt-6.3-sol")
	if count != 1 || like == nil {
		t.Fatalf("pro models contain gpt-6.3-sol %d times, want 1", count)
	}
	if clone.DisplayName != "GPT 6.3 Sol" || clone.ContextLength != like.ContextLength || len(clone.Thinking.Levels) != len(like.Thinking.Levels) {
		t.Fatalf("clone = %+v, want a copy of %+v", clone, like)
	}
	if _, count = countModels(GetCodexFreeModels(), "gpt-6.3-sol"); count != 0 {
		t.Fatal("free plan does not list gpt-6.2-sol, so it must not get its clone")
	}
	if info := LookupStaticModelInfoByChannel("gpt-6.3-sol", "codex"); info == nil {
		t.Fatal("static codex lookup misses the override model")
	}

	entry, count := codexClientModelEntry(t, "gpt-6.3-sol")
	likeEntry, _ := codexClientModelEntry(t, "gpt-6.2-sol")
	if count != 1 || entry.Get("display_name").String() != "GPT-6.3-Sol" {
		t.Fatalf("client catalog entry = %s", entry.Raw)
	}
	if entry.Get("base_instructions").String() != likeEntry.Get("base_instructions").String() || entry.Get("service_tiers").Raw != likeEntry.Get("service_tiers").Raw {
		t.Fatal("client catalog entry is not a clone of the like template")
	}
	if levels := CodexClientModelThinkingLevels("gpt-6.3-sol"); len(levels) == 0 {
		t.Fatal("thinking levels index misses the cloned template")
	}

	status := EvaluateCodexModelOverrides(currentCodexModelOverrides(), false)[0]
	if status.State.Model != CodexOverrideActive || status.State.Catalog != CodexOverrideActive {
		t.Fatalf("state = %+v, want active/active", status.State)
	}
}

func TestCodexModelOverrideDisplayName(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	ApplyCodexModelOverrides([]CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol", DisplayName: "Sol Next"}}, false)
	clone, _ := countModels(GetCodexProModels(), "gpt-6.3-sol")
	entry, _ := codexClientModelEntry(t, "gpt-6.3-sol")
	if clone == nil || clone.DisplayName != "Sol Next" || entry.Get("display_name").String() != "Sol Next" {
		t.Fatalf("display names = %v / %s", clone, entry.Get("display_name").Raw)
	}
}

func TestCodexModelOverrideUpstreamWins(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	overrides := []CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol", DisplayName: "Override"}}
	ApplyCodexModelOverrides(overrides, false)

	// Upstream adds the model to models.json only.
	doc := ojson.NewDoc(embeddedModelsJSON)
	upstreamModel := `{"id":"gpt-6.3-sol","object":"model","owned_by":"openai","type":"openai","display_name":"Upstream"}`
	if errSet := doc.SetRaw("codex-pro.-1", upstreamModel); errSet != nil {
		t.Fatal(errSet)
	}
	if _, errPublish := publishCatalogBytes(doc.Bytes()); errPublish != nil {
		t.Fatal(errPublish)
	}
	model, count := countModels(GetCodexProModels(), "gpt-6.3-sol")
	if count != 1 || model.DisplayName != "Upstream" {
		t.Fatalf("pro models = %v (%d), want only the upstream entry", model, count)
	}
	// No plan may get a clone once any plan lists the model upstream.
	if _, count = countModels(GetCodexTeamModels(), "gpt-6.3-sol"); count != 0 {
		t.Fatal("team plan got a clone although upstream lists the model")
	}
	state := EvaluateCodexModelOverrides(overrides, false)[0].State
	if state.Model != CodexOverrideUpstream || state.Catalog != CodexOverrideActive {
		t.Fatalf("state = %+v, want model upstream and catalog active", state)
	}

	// Upstream then adds the client template too.
	like, _ := codexClientModelEntry(t, "gpt-6.2-sol")
	template := ojson.NewDocString(like.Raw)
	_ = template.Set("slug", "gpt-6.3-sol")
	_ = template.Set("display_name", "Upstream Template")
	clientDoc := ojson.NewDoc(embeddedCodexClientModelsJSON)
	_ = clientDoc.SetRaw("models.-1", template.String())
	if _, errLoad := loadCodexClientModelsFromBytes(clientDoc.Bytes(), "test"); errLoad != nil {
		t.Fatal(errLoad)
	}
	entry, count := codexClientModelEntry(t, "gpt-6.3-sol")
	if count != 1 || entry.Get("display_name").String() != "Upstream Template" {
		t.Fatalf("client entry = %s (%d), want only the upstream template", entry.Raw, count)
	}
	if !bytes.Equal(GetCodexClientModelsJSON(), clientDoc.Bytes()) {
		t.Fatal("redundant override changed the upstream client catalog")
	}
	state = EvaluateCodexModelOverrides(overrides, false)[0].State
	if state.Model != CodexOverrideUpstream || state.Catalog != CodexOverrideUpstream {
		t.Fatalf("state = %+v, want upstream/upstream", state)
	}
}

func TestCodexModelOverrideAddsServiceTierOnce(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	before, _ := codexClientModelEntry(t, "gpt-6.2-astra")
	if bytes.Contains([]byte(before.Raw), []byte("ultrafast")) {
		t.Fatal("test expects gpt-6.2-astra without ultrafast upstream")
	}
	revision := GetCodexClientModelsRevision()
	ApplyCodexModelOverrides([]CodexModelOverride{{ID: "gpt-6.2-astra", AddServiceTiers: []string{"ultrafast", "Ultrafast"}}}, false)
	if GetCodexClientModelsRevision() == revision {
		t.Fatal("catalog revision did not change")
	}

	entry, _ := codexClientModelEntry(t, "gpt-6.2-astra")
	tiers := entry.Get("service_tiers").Array()
	if len(tiers) != 2 || tiers[0].Get("id").String() != "priority" || tiers[1].Raw != `{"id":"ultrafast","name":"Ultrafast","description":""}` {
		t.Fatalf("service_tiers = %s", entry.Get("service_tiers").Raw)
	}
	speed := entry.Get("additional_speed_tiers").Array()
	if len(speed) != 2 || speed[0].String() != "fast" || speed[1].String() != "ultrafast" {
		t.Fatalf("additional_speed_tiers = %s", entry.Get("additional_speed_tiers").Raw)
	}
	state := EvaluateCodexModelOverrides(currentCodexModelOverrides(), false)[0].State
	if state.ServiceTiers["ultrafast"] != CodexOverrideActive || state.Model != CodexOverrideNone || state.Catalog != CodexOverrideNone {
		t.Fatalf("state = %+v", state)
	}
}

func TestCodexModelOverrideServiceTierUpstreamNoop(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	before := GetCodexClientModelsJSON()
	overrides := []CodexModelOverride{{ID: "gpt-6.2-sol", AddServiceTiers: []string{"ultrafast", "fast"}}}
	ApplyCodexModelOverrides(overrides, false)
	if !bytes.Equal(before, GetCodexClientModelsJSON()) {
		t.Fatal("tier override changed a model that already has the tiers")
	}
	state := EvaluateCodexModelOverrides(overrides, false)[0].State
	if state.ServiceTiers["ultrafast"] != CodexOverrideUpstream || state.ServiceTiers["fast"] != CodexOverrideUpstream {
		t.Fatalf("state = %+v, want upstream tiers", state)
	}
}

func TestCodexModelOverrideInactiveWithoutLike(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	before := GetCodexClientModelsJSON()
	overrides := []CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-missing", AddServiceTiers: []string{"ultrafast"}}}
	ApplyCodexModelOverrides(overrides, false)
	if _, count := countModels(GetCodexProModels(), "gpt-6.3-sol"); count != 0 {
		t.Fatal("inactive override registered a model")
	}
	if !bytes.Equal(before, GetCodexClientModelsJSON()) {
		t.Fatal("inactive override changed the client catalog")
	}
	state := EvaluateCodexModelOverrides(overrides, false)[0].State
	if state.Model != CodexOverrideInactive || state.Catalog != CodexOverrideInactive || state.ServiceTiers["ultrafast"] != CodexOverrideInactive {
		t.Fatalf("state = %+v, want inactive", state)
	}
}

func TestCodexModelOverrideRemovalAndHome(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	before := GetCodexClientModelsJSON()
	overrides := []CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol"}}
	ApplyCodexModelOverrides(overrides, false)
	ApplyCodexModelOverrides(nil, false)
	if _, count := countModels(GetCodexProModels(), "gpt-6.3-sol"); count != 0 || !bytes.Equal(before, GetCodexClientModelsJSON()) {
		t.Fatal("removing the override left it applied")
	}
	ApplyCodexModelOverrides(overrides, true)
	if _, count := countModels(GetCodexProModels(), "gpt-6.3-sol"); count != 0 {
		t.Fatal("Home mode applied an override")
	}
	if state := EvaluateCodexModelOverrides(overrides, true)[0].State; state.Model != CodexOverrideInactive {
		t.Fatalf("Home state = %+v, want inactive", state)
	}
}

func TestCodexModelOverrideNotifiesCodexRefresh(t *testing.T) {
	useCodexOverrideTestCatalogs(t)
	changes := make(chan []string, 4)
	SetModelRefreshCallback(func(providers []string) { changes <- providers })
	t.Cleanup(func() { SetModelRefreshCallback(nil) })

	ApplyCodexModelOverrides([]CodexModelOverride{{ID: "gpt-6.3-sol", Like: "gpt-6.2-sol"}}, false)
	select {
	case providers := <-changes:
		if len(providers) != 1 || providers[0] != "codex" {
			t.Fatalf("providers = %v, want [codex]", providers)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh notification")
	}
	// Re-applying the same list (after normalization) is a no-op.
	revision := GetCodexClientModelsRevision()
	ApplyCodexModelOverrides([]CodexModelOverride{{ID: " gpt-6.3-sol ", Like: "gpt-6.2-sol"}}, false)
	if GetCodexClientModelsRevision() != revision {
		t.Fatal("re-applying the same overrides republished the catalog")
	}
}

func TestValidateCodexModelOverrides(t *testing.T) {
	invalid := [][]CodexModelOverride{
		{{Like: "gpt-6.2-sol"}},
		{{ID: "gpt-6.3-sol"}},
		{{ID: "gpt-6.3-sol", Like: "GPT-6.3-SOL"}},
		{{ID: "gpt-6.3-sol", AddServiceTiers: []string{" "}}},
		{{ID: "a", Like: "b"}, {ID: "a", AddServiceTiers: []string{"ultrafast"}}},
	}
	for _, entries := range invalid {
		if errValidate := ValidateCodexModelOverrides(entries); errValidate == nil {
			t.Fatalf("accepted %+v", entries)
		}
	}
	if errValidate := ValidateCodexModelOverrides([]CodexModelOverride{{ID: "a", Like: "b"}, {ID: "c", AddServiceTiers: []string{"ultrafast"}}}); errValidate != nil {
		t.Fatal(errValidate)
	}
}
