package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const testDrainOwner = "0123456789abcdef0123456789abcdef"

func newDrainServer(t *testing.T) *Server {
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.RemoteManagement.SecretKey = "unused-hash-for-local-password"
	return newTestServerWithConfig(t, cfg, WithLocalManagementPassword("test-admin"))
}

func drainHTTP(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Config = s.server
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func drainCall(t *testing.T, ts *httptest.Server, method, path, instance, token string) (int, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"instance_id":%q,"owner_token":%q}`, instance, token)
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-admin")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestDrainManagementAuthenticationAndRecovery(t *testing.T) {
	s := newDrainServer(t)
	ts := drainHTTP(t, s)
	resp, err := ts.Client().Get(ts.URL + drainPath)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	_ = resp.Body.Close()
	code, data := drainCall(t, ts, "GET", drainPath, "", "")
	if code != 200 || !ojson.GetBytesNoCopy(data, "supported").Bool() {
		t.Fatal(code, string(data))
	}
	instance := ojson.GetBytesNoCopy(data, "instance_id").String()
	code, data = drainCall(t, ts, "POST", drainPath, instance, testDrainOwner)
	if code != 200 || !ojson.GetBytesNoCopy(data, "ready").Bool() {
		t.Fatal(code, string(data))
	}
	if strings.Contains(string(data), testDrainOwner) {
		t.Fatal("status leaked owner")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/backend-api/codex/responses", nil)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatal(resp.StatusCode)
	}
	_ = resp.Body.Close()
	if code, _ = drainCall(t, ts, "DELETE", drainPath, instance, strings.Repeat("x", 32)); code != 409 {
		t.Fatal(code)
	}
	if code, _ = drainCall(t, ts, "DELETE", drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 503 {
		t.Fatal("cancel did not resume admission")
	}
	_ = resp.Body.Close()
}

func TestDrainedStopWaitsForStreamingCustomRoute(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	s := newDrainServer(t)
	s.engine.POST("/custom-stream", func(c *gin.Context) {
		c.Status(200)
		c.Writer.Flush()
		close(started)
		<-finish
		c.String(200, "complete")
	})
	ts := drainHTTP(t, s)
	resp, err := ts.Client().Post(ts.URL+"/custom-stream", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	instance := s.drain.Status().InstanceID
	if code, _ := drainCall(t, ts, "POST", drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	if code, _ := drainCall(t, ts, "POST", drainPath+"/stop", instance, testDrainOwner); code != 409 {
		t.Fatal("stopped active response", code)
	}
	once.Do(func() { close(finish) })
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "complete" {
		t.Fatal(string(body), err)
	}
	// A health request on the same HTTP/1 connection runs only after final flush.
	health, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = health.Body.Close()
	if code, _ := drainCall(t, ts, "POST", drainPath+"/stop", instance, testDrainOwner); code != 202 {
		t.Fatal(code)
	}
	if !s.drain.Status().Stopping {
		t.Fatal("stop not permanently fenced")
	}
}

type drainWebsocketExecutor struct {
	serverResponsesWebsocketCaptureExecutor
	tools   bool
	started chan struct{}
	finish  chan struct{}
}

func (e *drainWebsocketExecutor) ExecuteStream(ctx context.Context, _ *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	n := e.calls.Add(1)
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		if e.started != nil && n == 1 {
			close(e.started)
			select {
			case <-ctx.Done():
				return
			case <-e.finish:
			}
		}
		output := `[]`
		if e.tools && n == 1 {
			output = `[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}"}]`
		}
		select {
		case <-ctx.Done():
		case chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","output":%s}}`, n, output))}:
		}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestDrainResponsesWebsocket(t *testing.T) {
	for _, pendingTools := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_tools_%v", pendingTools), func(t *testing.T) {
			s := newDrainServer(t)
			exec := &drainWebsocketExecutor{tools: pendingTools, started: make(chan struct{}), finish: make(chan struct{})}
			s.handlers.AuthManager.RegisterExecutor(exec)
			entry := &auth.Auth{ID: "drain-auth", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"websockets": "true"}}
			_, err := s.handlers.AuthManager.Register(context.Background(), entry)
			if err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(entry.ID, entry.Provider, []*registry.ModelInfo{{ID: "drain-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(entry.ID) })
			ts := drainHTTP(t, s)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-key"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var finishOnce sync.Once
			defer finishOnce.Do(func() { close(exec.finish) })
			turn := []byte(`{"type":"response.create","model":"drain-model","input":[{"role":"user","content":"hello"}]}`)
			if err = conn.WriteMessage(websocket.TextMessage, turn); err != nil {
				t.Fatal(err)
			}
			<-exec.started
			instance := s.drain.Status().InstanceID
			if code, _ := drainCall(t, ts, "POST", drainPath, instance, testDrainOwner); code != 200 {
				t.Fatal(code)
			}
			if s.drain.Status().Ready || s.drain.Status().ActiveWebsocketTurns != 1 {
				t.Fatal(s.drain.Status())
			}
			finishOnce.Do(func() { close(exec.finish) })
			// The active response must complete before the maintenance close.
			_, data, err := conn.ReadMessage()
			if err != nil || ojson.GetBytesNoCopy(data, "type").String() != "response.completed" {
				t.Fatal(string(data), err)
			}
			if pendingTools {
				// A tool call can be running outside the socket. Drain must not close it.
				if code, _ := drainCall(t, ts, "POST", drainPath+"/stop", instance, testDrainOwner); code != 409 {
					t.Fatal("stopped pending tools", code)
				}
				// Pending tools are admitted work. Their result may finish while draining.
				continuation := []byte(`{"type":"response.create","model":"drain-model","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call_1","output":"finished"}]}`)
				if err = conn.WriteMessage(websocket.TextMessage, continuation); err != nil {
					t.Fatal(err)
				}
				_, data, err = conn.ReadMessage()
				if err != nil || ojson.GetBytesNoCopy(data, "type").String() != "response.completed" {
					t.Fatal("tool continuation interrupted", string(data), err)
				}
				_, _, err = conn.ReadMessage()
				if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
					t.Fatal("missing replay close after tool completed", err)
				}
			} else {
				_, _, err = conn.ReadMessage()
				if !websocket.IsCloseError(err, websocket.CloseServiceRestart) {
					t.Fatal("missing replay close", err)
				}
			}
			// Cleanup includes asynchronous socket readers. Wait on a condition, not a
			// timing assertion, before allowing TempDir to remove request log folders.
			deadline := time.After(5 * time.Second)
			for s.drain.Status().WebsocketSessions != 0 {
				select {
				case <-deadline:
					t.Fatal("WebSocket cleanup did not finish")
				default:
				}
				// Management polling also gives the handler goroutine execution time.
				drainCall(t, ts, "GET", drainPath, "", "")
			}
			if !s.drain.Status().Ready {
				t.Fatal(s.drain.Status())
			}
		})
	}
}

func TestDrainUnknownBackgroundWebsocketWaitsForSocketClose(t *testing.T) {
	s := newDrainServer(t)
	release, closed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.AttachWebsocketRoute("/background-ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go func() { <-release; _ = conn.Close(); close(closed) }()
		// The production AI Studio relay returns here with a background owner too.
	}))
	ts := drainHTTP(t, s)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/background-ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	instance := s.drain.Status().InstanceID
	if code, _ := drainCall(t, ts, "POST", drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	if status := s.drain.Status(); status.Ready || status.ActiveRequests != 1 {
		t.Fatal("lost background socket", status)
	}
	if code, _ := drainCall(t, ts, "POST", drainPath+"/stop", instance, testDrainOwner); code != 409 {
		t.Fatal(code)
	}
	once.Do(func() { close(release) })
	<-closed
	// Close can finish before the outer logging/middleware chain returns.
	// Wait for both completion conditions, as the production updater does.
	deadline := time.After(5 * time.Second)
	for !s.drain.Status().Ready {
		select {
		case <-deadline:
			t.Fatal("background handler cleanup did not finish", s.drain.Status())
		default:
		}
		drainCall(t, ts, "GET", drainPath, "", "")
	}
}

func TestDrainConfiguredPluginBlocksMaintenance(t *testing.T) {
	s := newDrainServer(t)
	enabled := true
	s.cfg.Plugins.Enabled = true
	s.cfg.Plugins.Configs = map[string]proxyconfig.PluginInstanceConfig{"test": {Enabled: &enabled}}
	ts := drainHTTP(t, s)
	code, data := drainCall(t, ts, "POST", drainPath, s.drain.Status().InstanceID, testDrainOwner)
	if code != 409 || ojson.GetBytesNoCopy(data, "supported").Bool() || s.drain.Status().Draining {
		t.Fatal(code, string(data))
	}
}

func TestDrainFencesMutatingOAuthGETRequests(t *testing.T) {
	s := newDrainServer(t)
	ts := drainHTTP(t, s)
	if code, _ := drainCall(t, ts, "POST", drainPath, s.drain.Status().InstanceID, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	for _, path := range []string{
		"/v8/management/oauth/auth-url?provider=codex",
		"/v8/management/oauth/callback?state=test&code=test",
		"/v0/management/codex-auth-url",
		"/v0/management/oauth-callback?state=test&code=test",
	} {
		if code, _ := drainCall(t, ts, "GET", path, "", ""); code != 503 {
			t.Fatal("OAuth GET bypassed fence", path, code)
		}
	}
	// Configuration and the drain snapshot remain readable during maintenance.
	if code, _ := drainCall(t, ts, "GET", drainPath, "", ""); code != 200 {
		t.Fatal(code)
	}
}

func TestPendingOAuthSessionBlocksDrainAfterItsRequestReturns(t *testing.T) {
	s := newDrainServer(t)
	ts := drainHTTP(t, s)
	state := "maintenance-test-pending-oauth"
	managementHandlers.RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { managementHandlers.CancelOAuthSession(state) })
	instance := s.drain.Status().InstanceID
	code, data := drainCall(t, ts, "POST", drainPath, instance, testDrainOwner)
	if code != 409 || ojson.GetBytesNoCopy(data, "supported").Bool() {
		t.Fatal(code, string(data))
	}
	// Check the atomic commit path too: a previously ready drain must recheck
	// outstanding detached work even if a caller ignores the status snapshot.
	_, ok := s.drain.Begin(instance, testDrainOwner)
	if !ok {
		t.Fatal("cannot set test lease")
	}
	if code, _ := drainCall(t, ts, "POST", drainPath+"/stop", instance, testDrainOwner); code != 409 {
		t.Fatal("stopped pending OAuth", code)
	}
	managementHandlers.CancelOAuthSession(state)
	if !s.drain.Cancel(instance, testDrainOwner) {
		t.Fatal("cannot cancel test lease")
	}
	if code, _ := drainCall(t, ts, "POST", drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
}
