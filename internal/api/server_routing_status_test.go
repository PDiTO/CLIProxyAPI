package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRoutingStatusManagementAuthenticationAndContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, err := config.ParseConfigBytes([]byte("remote-management: {secret-key: test-password}\n"))
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, &coreauth.SoonestResetSelector{}, nil)
	h := management.NewHandler(cfg, "", manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	for _, tc := range []struct {
		path       string
		authorized bool
		want       int
	}{
		{"/v8/management/routing/status?provider=codex", false, 401},
		{"/v8/management/routing/status?provider=codex", true, 200},
		{"/v8/management/routing/status?provider=codex&transport=websocket", true, 200},
		{"/v8/management/routing/status", true, 400},
		{"/v8/management/routing/status?provider=codex&transport=invalid", true, 400},
		{"/v0/management/routing/status?provider=codex", true, 404},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if tc.authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s status=%d want=%d body=%s", tc.path, rec.Code, tc.want, rec.Body.String())
		}
		if rec.Code == 200 {
			data := ojson.Parse(rec.Body.String())
			if data.Get("schema_version").Int() != 1 || data.Get("strategy").String() != "soonest-reset-first" || data.Get("prediction_scope").String() != "new_session" || data.Get("fallback_reason").String() != "no_eligible_accounts" || !data.Get("accounts").IsArray() {
				t.Fatalf("wrong status contract: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "test-password") || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("status not private")
			}
		}
	}
}
