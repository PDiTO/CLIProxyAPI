package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (h *Handler) GetRoutingStatus(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	model := strings.TrimSpace(c.Query("model"))
	if provider == "" || len(provider) > 64 || len(model) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required; provider/model too long"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "routing manager unavailable"})
		return
	}
	ctx := c.Request.Context()
	switch c.DefaultQuery("transport", "http") {
	case "http":
	case "websocket":
		ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid transport"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, h.authManager.RoutingStatus(ctx, provider, model, time.Now()))
}
