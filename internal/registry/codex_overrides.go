package registry

// Fork-only: temporary Codex model and service tier overrides that fill gaps in
// the upstream catalogs until upstream catches up. Upstream always wins: each
// part of an override steps aside as soon as the upstream catalog provides it.

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

// CodexModelOverride is one models.codex-overrides entry.
type CodexModelOverride struct {
	// ID is the Codex model the override supplies or extends.
	ID string `yaml:"id" json:"id"`
	// Like names an existing model whose limits, thinking levels and Codex
	// client template are cloned when upstream does not list ID yet.
	Like string `yaml:"like,omitempty" json:"like,omitempty"`
	// DisplayName optionally names a cloned model.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	// AddServiceTiers lists Codex client service tiers to add to ID.
	AddServiceTiers []string `yaml:"add-service-tiers,omitempty" json:"add-service-tiers,omitempty"`
}

// Override states reported by EvaluateCodexModelOverrides.
const (
	CodexOverrideActive   = "active"
	CodexOverrideUpstream = "upstream"
	CodexOverrideInactive = "inactive"
	CodexOverrideNone     = "none"
)

// CodexModelOverrideState reports what each part of an override does now.
type CodexModelOverrideState struct {
	Model        string            `json:"model"`
	Catalog      string            `json:"catalog"`
	ServiceTiers map[string]string `json:"service-tiers"`
}

// CodexModelOverrideStatus is an override entry with its current state.
type CodexModelOverrideStatus struct {
	ID              string                  `json:"id"`
	Like            string                  `json:"like"`
	DisplayName     string                  `json:"display-name"`
	AddServiceTiers []string                `json:"add-service-tiers"`
	State           CodexModelOverrideState `json:"state"`
}

var codexOverrideStore struct {
	sync.RWMutex
	entries []CodexModelOverride
}

// ValidateCodexModelOverrides rejects entries that cannot describe an override.
func ValidateCodexModelOverrides(entries []CodexModelOverride) error {
	seen := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			return fmt.Errorf("models.codex-overrides[%d].id is required", i)
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("models.codex-overrides contains duplicate id %q", id)
		}
		seen[key] = struct{}{}
		like := strings.TrimSpace(entry.Like)
		if like == "" && len(entry.AddServiceTiers) == 0 {
			return fmt.Errorf("models.codex-overrides[%d] (%s) needs like or add-service-tiers", i, id)
		}
		if strings.EqualFold(like, id) {
			return fmt.Errorf("models.codex-overrides[%d] (%s): like must differ from id", i, id)
		}
		for _, tier := range entry.AddServiceTiers {
			if strings.TrimSpace(tier) == "" {
				return fmt.Errorf("models.codex-overrides[%d] (%s) has an empty service tier", i, id)
			}
		}
	}
	return nil
}

// ApplyCodexModelOverrides replaces the active overrides. Home mode serves its
// own catalogs, so overrides are cleared there. Invalid lists are ignored.
// When the list changes, the Codex client catalog is republished at once and
// Codex auths are re-registered in the background.
func ApplyCodexModelOverrides(entries []CodexModelOverride, home bool) {
	if home {
		entries = nil
	}
	if errValidate := ValidateCodexModelOverrides(entries); errValidate != nil {
		log.WithError(errValidate).Warn("ignoring invalid codex model overrides")
		return
	}
	normalized := normalizeCodexModelOverrides(entries)

	codexOverrideStore.Lock()
	if reflect.DeepEqual(codexOverrideStore.entries, normalized) {
		codexOverrideStore.Unlock()
		return
	}
	codexOverrideStore.entries = normalized
	codexOverrideStore.Unlock()

	codexClientCatalogStore.mu.Lock()
	publishCodexClientModelsLocked()
	codexClientCatalogStore.mu.Unlock()

	for _, status := range EvaluateCodexModelOverrides(normalized, false) {
		if status.State.Model == CodexOverrideInactive || status.State.Catalog == CodexOverrideInactive {
			log.Warnf("codex model override %s: like model %s is missing upstream; override is inactive", status.ID, status.Like)
		}
	}
	log.Infof("codex model overrides updated (%d entries)", len(normalized))
	// Before the service registers its callback there are no Codex auths to
	// update, and queuing would replay a stale change later.
	refreshCallbackMu.Lock()
	cb := refreshCallback
	refreshCallbackMu.Unlock()
	if cb != nil {
		// Re-register outside the caller's locks; config reloads hold service locks.
		go cb([]string{"codex"})
	}
}

func normalizeCodexModelOverrides(entries []CodexModelOverride) []CodexModelOverride {
	if len(entries) == 0 {
		return nil
	}
	out := make([]CodexModelOverride, 0, len(entries))
	for _, entry := range entries {
		normalized := CodexModelOverride{
			ID:          strings.TrimSpace(entry.ID),
			Like:        strings.TrimSpace(entry.Like),
			DisplayName: strings.TrimSpace(entry.DisplayName),
		}
		seenTiers := make(map[string]struct{}, len(entry.AddServiceTiers))
		for _, tier := range entry.AddServiceTiers {
			tier = strings.ToLower(strings.TrimSpace(tier))
			serviceID, _, _ := codexServiceTier(tier)
			if _, exists := seenTiers[serviceID]; exists {
				continue
			}
			seenTiers[serviceID] = struct{}{}
			normalized.AddServiceTiers = append(normalized.AddServiceTiers, tier)
		}
		out = append(out, normalized)
	}
	return out
}

func currentCodexModelOverrides() []CodexModelOverride {
	codexOverrideStore.RLock()
	defer codexOverrideStore.RUnlock()
	return codexOverrideStore.entries
}

// EvaluateCodexModelOverrides reports the state of entries against the current
// upstream catalogs, whether or not the entries are active.
func EvaluateCodexModelOverrides(entries []CodexModelOverride, home bool) []CodexModelOverrideStatus {
	normalized := normalizeCodexModelOverrides(entries)
	codexClientCatalogStore.mu.RLock()
	upstream := codexClientCatalogStore.upstream
	codexClientCatalogStore.mu.RUnlock()
	_, clientStates := applyCodexClientOverrides(upstream, normalized)
	data := getModels()

	statuses := make([]CodexModelOverrideStatus, 0, len(normalized))
	for i, entry := range normalized {
		status := CodexModelOverrideStatus{
			ID:              entry.ID,
			Like:            entry.Like,
			DisplayName:     entry.DisplayName,
			AddServiceTiers: append([]string{}, entry.AddServiceTiers...),
			State:           clientStates[i],
		}
		status.State.Model = codexRegistryOverrideState(data, entry)
		if home {
			status.State = inactiveCodexOverrideState(entry)
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func inactiveCodexOverrideState(entry CodexModelOverride) CodexModelOverrideState {
	state := CodexModelOverrideState{Model: CodexOverrideNone, Catalog: CodexOverrideNone, ServiceTiers: map[string]string{}}
	if entry.Like != "" {
		state.Model = CodexOverrideInactive
		state.Catalog = CodexOverrideInactive
	}
	for _, tier := range entry.AddServiceTiers {
		state.ServiceTiers[tier] = CodexOverrideInactive
	}
	return state
}

func codexPlanLists(data *staticModelsJSON) [][]*ModelInfo {
	if data == nil {
		return nil
	}
	return [][]*ModelInfo{data.CodexFree, data.CodexTeam, data.CodexPlus, data.CodexPro}
}

func codexPlansContain(data *staticModelsJSON, id string) bool {
	for _, list := range codexPlanLists(data) {
		if findStaticModel(list, id) != nil {
			return true
		}
	}
	return false
}

func codexRegistryOverrideState(data *staticModelsJSON, entry CodexModelOverride) string {
	switch {
	case entry.Like == "":
		return CodexOverrideNone
	case codexPlansContain(data, entry.ID):
		return CodexOverrideUpstream
	case codexPlansContain(data, entry.Like):
		return CodexOverrideActive
	default:
		return CodexOverrideInactive
	}
}

// withCodexModelOverrides appends clones for override models that no Codex
// plan in data lists yet, to a plan list that contains their like model.
// It never modifies list in place.
func withCodexModelOverrides(data *staticModelsJSON, list []*ModelInfo) []*ModelInfo {
	entries := currentCodexModelOverrides()
	if len(entries) == 0 {
		return list
	}
	var out []*ModelInfo
	for _, entry := range entries {
		if entry.Like == "" || codexPlansContain(data, entry.ID) {
			continue
		}
		like := findStaticModel(list, entry.Like)
		if like == nil {
			continue
		}
		if out == nil {
			out = append(make([]*ModelInfo, 0, len(list)+len(entries)), list...)
		}
		clone := cloneModelInfo(like)
		clone.ID = entry.ID
		clone.DisplayName = entry.DisplayName
		if clone.DisplayName == "" {
			clone.DisplayName = deriveCodexDisplayName(entry.ID, " ")
		}
		clone.SourceModelID = ""
		clone.MetadataModelID = ""
		out = append(out, clone)
	}
	if out == nil {
		return list
	}
	return out
}

// deriveCodexDisplayName turns "gpt-6.3-sol" into "GPT 6.3 Sol" (sep " ") or
// "GPT-6.3-Sol" (sep "-"), matching the upstream catalog styles.
func deriveCodexDisplayName(id, sep string) string {
	parts := strings.Split(id, "-")
	for i, part := range parts {
		if strings.EqualFold(part, "gpt") {
			parts[i] = "GPT"
			continue
		}
		parts[i] = titleCodexWord(part)
	}
	return strings.Join(parts, sep)
}

func titleCodexWord(word string) string {
	if word == "" {
		return word
	}
	return strings.ToUpper(word[:1]) + word[1:]
}

// codexServiceTier maps a configured tier to its service_tiers id, its
// additional_speed_tiers id and its display name. Upstream lists the
// "priority" service tier as the "fast" speed tier; other tiers use one id.
func codexServiceTier(tier string) (serviceID, speedID, name string) {
	if tier == "fast" || tier == "priority" {
		return "priority", "fast", "Fast"
	}
	return tier, tier, titleCodexWord(tier)
}

func codexEntryHasTier(entry ojson.Result, serviceID, speedID string) (hasService, hasSpeed bool) {
	for _, tier := range entry.Get("service_tiers").Array() {
		if tier.Get("id").String() == serviceID {
			hasService = true
		}
	}
	for _, tier := range entry.Get("additional_speed_tiers").Array() {
		if tier.String() == speedID {
			hasSpeed = true
		}
	}
	return hasService, hasSpeed
}

// applyCodexClientOverrides returns the Codex client catalog with overrides
// applied, plus each entry's catalog and service tier state. With nothing to
// change it returns upstream itself.
func applyCodexClientOverrides(upstream []byte, entries []CodexModelOverride) ([]byte, []CodexModelOverrideState) {
	states := make([]CodexModelOverrideState, len(entries))
	if len(entries) == 0 {
		return upstream, states
	}
	models := ojson.GetBytes(upstream, "models").Array()
	index := make(map[string]int, len(models))
	for i, model := range models {
		index[model.Get("slug").String()] = i
	}

	// All edits stay in one document, serialized once at the return.
	doc := ojson.NewDoc(upstream)
	changed := false
	next := len(models)
	for i, entry := range entries {
		state := CodexModelOverrideState{Model: CodexOverrideNone, Catalog: CodexOverrideNone, ServiceTiers: map[string]string{}}
		upstreamIdx, upstreamHas := index[entry.ID]

		// Tier edits go to the upstream entry or to a clone appended to models.
		path, clone := "", false
		if upstreamHas {
			path = "models." + strconv.Itoa(upstreamIdx)
		}
		if entry.Like != "" {
			likeIdx, likeHas := index[entry.Like]
			switch {
			case upstreamHas:
				state.Catalog = CodexOverrideUpstream
			case likeHas:
				state.Catalog = CodexOverrideActive
				displayName := entry.DisplayName
				if displayName == "" {
					displayName = deriveCodexDisplayName(entry.ID, "-")
				}
				path, clone = "models."+strconv.Itoa(next), true
				next++
				_ = doc.SetRaw(path, models[likeIdx].Raw)
				_ = doc.Set(path+".slug", entry.ID)
				_ = doc.Set(path+".display_name", displayName)
				changed = true
			default:
				state.Catalog = CodexOverrideInactive
			}
		}

		for _, tier := range entry.AddServiceTiers {
			if path == "" {
				state.ServiceTiers[tier] = CodexOverrideInactive
				continue
			}
			current := doc.Get(path)
			serviceID, speedID, name := codexServiceTier(tier)
			hasService, hasSpeed := codexEntryHasTier(current, serviceID, speedID)
			if hasService && hasSpeed {
				if clone {
					state.ServiceTiers[tier] = CodexOverrideActive
				} else {
					state.ServiceTiers[tier] = CodexOverrideUpstream
				}
				continue
			}
			state.ServiceTiers[tier] = CodexOverrideActive
			if !hasService {
				tiers := current.Get("service_tiers")
				if !tiers.IsArray() {
					_ = doc.SetRaw(path+".service_tiers", "[]")
				}
				// Set members one by one to keep upstream's key order.
				item := path + ".service_tiers." + strconv.Itoa(len(tiers.Array()))
				_ = doc.SetRaw(item, "{}")
				_ = doc.Set(item+".id", serviceID)
				_ = doc.Set(item+".name", name)
				_ = doc.Set(item+".description", "")
			}
			if !hasSpeed {
				if !current.Get("additional_speed_tiers").IsArray() {
					_ = doc.SetRaw(path+".additional_speed_tiers", "[]")
				}
				_ = doc.Set(path+".additional_speed_tiers.-1", speedID)
			}
			changed = true
		}
		states[i] = state
	}
	if !changed {
		return upstream, states
	}
	return doc.Bytes(), states
}
