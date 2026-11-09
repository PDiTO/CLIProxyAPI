package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const drainPath = "/v8/management/server/drain"

func (s *Server) drainExempt(r *http.Request) bool {
	path := r.URL.Path
	if path == drainPath || path == drainPath+"/stop" {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return path == "/" || path == "/healthz" || path == "/management.html" ||
		strings.HasPrefix(path, "/v8/management/") || strings.HasPrefix(path, "/v0/management/")
}

func (s *Server) drainBlockers() []string {
	blockers := make([]string, 0)
	cfg := s.getConfig()
	if cfg == nil {
		return append(blockers, "configuration_unavailable")
	}
	if cfg.Home.Enabled || (s.handlers.AuthManager != nil && s.handlers.AuthManager.HomeEnabled()) {
		blockers = append(blockers, "home_enabled")
	}
	// Plugins can own work outside HTTP request lifetimes. Do not claim to drain it.
	if cfg.Plugins.Enabled {
		for _, item := range cfg.Plugins.Configs {
			if item.Enabled != nil && *item.Enabled {
				blockers = append(blockers, "plugins_enabled")
				break
			}
		}
	}
	if s.pluginHost != nil && s.pluginHost.HasLoadedPlugins() {
		blockers = append(blockers, "plugin_work_untracked")
	}
	if s.codexLiveHandler != nil && s.codexLiveHandler.ActiveSessions() > 0 {
		blockers = append(blockers, "realtime_sessions")
	}
	return blockers
}

func (s *Server) writeDrainStatus(c *gin.Context, code int) {
	status := s.drain.Status()
	blockers := s.drainBlockers()
	doc := ojson.NewDocString(`{}`)
	for key, value := range map[string]any{
		"schema_version": status.SchemaVersion, "instance_id": status.InstanceID,
		"draining": status.Draining, "stopping": status.Stopping, "lease_remaining_ms": status.LeaseRemainingMS,
		"active_requests": status.ActiveRequests, "websocket_sessions": status.WebsocketSessions,
		"active_websocket_turns": status.ActiveWebsocketTurns, "supported": len(blockers) == 0,
		"blockers": blockers, "ready": status.Ready && len(blockers) == 0,
	} {
		_ = doc.Set(key, value)
	}
	c.Header("Cache-Control", "no-store")
	body := doc.Bytes()
	// The stop handler flushes before closing the server. An explicit length
	// makes its reply complete without waiting for a final chunk after return.
	c.Header("Content-Length", strconv.Itoa(len(body)))
	c.Data(code, "application/json", body)
}

func drainOwner(c *gin.Context) (string, string, bool) {
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 4097))
	if errRead != nil || len(body) > 4096 || !ojson.ValidBytes(body) {
		c.String(http.StatusBadRequest, "invalid drain request")
		return "", "", false
	}
	doc := ojson.NewDoc(body)
	instance, owner := doc.Get("instance_id").String(), doc.Get("owner_token").String()
	if len(instance) != 36 || len(owner) < 32 || len(owner) > 128 {
		c.String(http.StatusBadRequest, "invalid drain owner")
		return "", "", false
	}
	return instance, owner, true
}

func (s *Server) getDrain(c *gin.Context) { s.writeDrainStatus(c, http.StatusOK) }

func (s *Server) beginDrain(c *gin.Context) {
	instance, owner, ok := drainOwner(c)
	if !ok {
		return
	}
	if len(s.drainBlockers()) > 0 {
		s.writeDrainStatus(c, http.StatusConflict)
		return
	}
	if _, ok = s.drain.Begin(instance, owner); !ok {
		s.writeDrainStatus(c, http.StatusConflict)
		return
	}
	s.writeDrainStatus(c, http.StatusOK)
}

func (s *Server) cancelDrain(c *gin.Context) {
	instance, owner, ok := drainOwner(c)
	if !ok {
		return
	}
	if !s.drain.Cancel(instance, owner) {
		s.writeDrainStatus(c, http.StatusConflict)
		return
	}
	s.writeDrainStatus(c, http.StatusOK)
}

// Stop is committed inside the proxy, rather than by killing a PID after a GET.
// This prevents lease expiration from admitting a response between ready and stop.
// A supervisor must restart the process on its newly selected retained binary.
func (s *Server) stopDrained(c *gin.Context) {
	instance, owner, ok := drainOwner(c)
	if !ok {
		return
	}
	if len(s.drainBlockers()) > 0 || !s.drain.CommitStop(instance, owner) {
		s.writeDrainStatus(c, http.StatusConflict)
		return
	}
	s.writeDrainStatus(c, http.StatusAccepted)
	c.Writer.Flush()
	go func() {
		if errStop := s.Stop(context.Background()); errStop != nil {
			log.WithError(errStop).Error("failed to stop drained API server")
		}
	}()
}
