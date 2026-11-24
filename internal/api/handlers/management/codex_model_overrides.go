package management

// Fork-only: status and replacement of models.codex-overrides.

import (
	"io"
	"net/http"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const maxCodexModelOverridesBody = 1 << 20

type codexModelOverridesResponse struct {
	Overrides []registry.CodexModelOverrideStatus `json:"overrides"`
}

// GetCodexModelOverrides reports each configured override and its state.
func (h *Handler) GetCodexModelOverrides(c *gin.Context) {
	h.mu.Lock()
	entries := append([]registry.CodexModelOverride(nil), h.cfg.Models.CodexOverrides...)
	home := h.cfg.Home.Enabled
	h.mu.Unlock()
	writeCodexModelOverrides(c, entries, home)
}

// PutCodexModelOverrides validates and replaces the override list, saves the
// config file and applies the new list at once.
func (h *Handler) PutCodexModelOverrides(c *gin.Context) {
	data, errRead := io.ReadAll(io.LimitReader(c.Request.Body, maxCodexModelOverridesBody+1))
	if errRead != nil || len(data) > maxCodexModelOverridesBody {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var body struct {
		Overrides *[]registry.CodexModelOverride `json:"overrides"`
	}
	if errUnmarshal := json.Unmarshal(data, &body); errUnmarshal != nil || body.Overrides == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: overrides array is required"})
		return
	}
	entries := *body.Overrides
	if len(entries) == 0 {
		entries = nil
	}
	if errValidate := registry.ValidateCodexModelOverrides(entries); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}

	h.mu.Lock()
	previous := h.cfg.Models.CodexOverrides
	h.cfg.Models.CodexOverrides = entries
	snapshot, okSave := h.saveConfigAndSnapshotLocked(c)
	if !okSave {
		h.cfg.Models.CodexOverrides = previous
		h.mu.Unlock()
		return
	}
	home := h.cfg.Home.Enabled
	h.mu.Unlock()

	// The service reload applies the list too; applying here first makes the
	// response reflect it even without a reload hook.
	registry.ApplyCodexModelOverrides(entries, home)
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	writeCodexModelOverrides(c, entries, home)
}

func writeCodexModelOverrides(c *gin.Context, entries []registry.CodexModelOverride, home bool) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, codexModelOverridesResponse{Overrides: registry.EvaluateCodexModelOverrides(entries, home)})
}
