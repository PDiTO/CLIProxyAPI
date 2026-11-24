package models

import (
	"reflect"
	"testing"
)

func TestFilterByProviders(t *testing.T) {
	models := []map[string]any{
		{"id": "gpt-codex"},
		{"id": "claude-only"},
		{"id": "mixed"},
		{"id": "unknown"},
		{"id": "team/gpt-codex"},
		{"id": " "},
	}
	providers := map[string][]string{
		"gpt-codex":   {"codex"},
		"claude-only": {"claude"},
		"mixed":       {"claude", "Codex"},
	}
	providersForModel := func(id string) []string { return providers[id] }
	ids := func(models []map[string]any) []string {
		out := make([]string, 0, len(models))
		for _, model := range models {
			out = append(out, stringModelValue(model, "id"))
		}
		return out
	}

	for _, tt := range []struct {
		name    string
		allowed string
		lookup  ProvidersForModelFunc
		want    []string
	}{
		{name: "empty list keeps every model", allowed: "", lookup: providersForModel, want: ids(models)},
		{name: "blank entries keep every model", allowed: " , ", lookup: providersForModel, want: ids(models)},
		{name: "nil lookup keeps every model", allowed: "codex", lookup: nil, want: ids(models)},
		{name: "codex only", allowed: " CODEX ", lookup: providersForModel, want: []string{"gpt-codex", "mixed", "team/gpt-codex"}},
		{name: "several providers", allowed: "codex, claude", lookup: providersForModel, want: []string{"gpt-codex", "claude-only", "mixed", "team/gpt-codex"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids(FilterByProviders(models, tt.allowed, tt.lookup)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FilterByProviders() = %v, want %v", got, tt.want)
			}
		})
	}
}
