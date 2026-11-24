package models

import "strings"

// FilterByProviders keeps the models served by at least one provider in the
// comma-separated allowed list. Provider names match case-insensitively. An
// empty list, or a nil providersForModel, returns models unchanged.
func FilterByProviders(models []map[string]any, allowed string, providersForModel ProvidersForModelFunc) []map[string]any {
	allowedSet := make(map[string]struct{})
	for _, provider := range strings.Split(allowed, ",") {
		if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
			allowedSet[provider] = struct{}{}
		}
	}
	if len(allowedSet) == 0 || providersForModel == nil {
		return models
	}

	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(stringModelValue(model, "id"))
		if id == "" {
			continue
		}
		for _, provider := range codexClientModelProviders(id, providersForModel) {
			if _, ok := allowedSet[strings.ToLower(strings.TrimSpace(provider))]; ok {
				filtered = append(filtered, model)
				break
			}
		}
	}
	return filtered
}
