package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Opt-in client compatibility smoke. Uses an isolated Codex home and a local
// executor only: no provider credentials, network quota or live proxy changes.
type drainCodexExecutor struct {
	serverResponsesWebsocketCaptureExecutor
	mu       sync.Mutex
	payloads []string
}

func (e *drainCodexExecutor) ExecuteStream(_ context.Context, _ *auth.Auth, request coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, string(request.Payload))
	n := len(e.payloads)
	e.mu.Unlock()
	item := fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"marker acknowledged","annotations":[]}]}`, n)
	events := []string{
		fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d","object":"response","status":"in_progress","output":[]}}`, n),
		fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_%d","type":"message","role":"assistant","content":[]}}`, n),
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"marker acknowledged"}`,
		fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, item),
		fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","object":"response","status":"completed","model":"gpt-5.4","output":[%s],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`, n, item),
	}
	chunks := make(chan coreexecutor.StreamChunk, len(events))
	for _, event := range events {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(event)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestDrainCodexClientReplaysConversation(t *testing.T) {
	if os.Getenv("CLIPROXY_CODEX_DRAIN_SMOKE") != "1" {
		t.Skip("set CLIPROXY_CODEX_DRAIN_SMOKE=1 with codex on PATH")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	s := newDrainServer(t)
	executor := &drainCodexExecutor{}
	s.handlers.AuthManager.RegisterExecutor(executor)
	entry := &auth.Auth{ID: "drain-codex-client", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"websockets": "true"}}
	_, err = s.handlers.AuthManager.Register(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(entry.ID, entry.Provider, []*registry.ModelInfo{{ID: "gpt-5.4", ContextLength: 128000}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(entry.ID) })
	ts := drainHTTP(t, s)
	home := t.TempDir()
	config := fmt.Sprintf(`model_provider = "localdrain"
model = "gpt-5.4"
[model_providers.localdrain]
name = "Isolated drain smoke"
base_url = %q
experimental_bearer_token = "test-key"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
`, ts.URL+"/v1")
	if err = os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := os.Create(filepath.Join(home, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	cmd.Stderr = diagnostics
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); cancel(); _ = cmd.Wait() }()
	messages := make(chan string, 128)
	go func() {
		defer close(messages)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			select {
			case messages <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(line string) {
		t.Helper()
		if _, errWrite := fmt.Fprintln(input, line); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	wait := func(match func(string) bool) string {
		t.Helper()
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line, ok := <-messages:
				if !ok {
					data, _ := os.ReadFile(filepath.Join(home, "stderr.log"))
					t.Fatalf("Codex exited: %s", data)
				}
				if match(line) {
					return line
				}
			case <-timer.C:
				data, _ := os.ReadFile(filepath.Join(home, "stderr.log"))
				t.Fatalf("Codex timed out: %s", data)
			}
		}
	}
	response := func(id int) string {
		t.Helper()
		line := wait(func(line string) bool { return int(ojson.Get(line, "id").Int()) == id })
		if ojson.Get(line, "error").Exists() {
			t.Fatal(line)
		}
		return line
	}
	send(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"drain-smoke","version":"1.0"},"capabilities":{"experimentalApi":true}}}`)
	response(1)
	send(`{"method":"initialized","params":{}}`)
	send(fmt.Sprintf(`{"id":2,"method":"thread/start","params":{"cwd":%q,"ephemeral":true,"approvalPolicy":"never","sandbox":"read-only","baseInstructions":"Answer with a short acknowledgment. Do not use tools."}}`, home))
	thread := ojson.Get(response(2), "result.thread.id").String()
	if thread == "" {
		t.Fatal("missing thread")
	}
	turn := func(id int, text string) {
		t.Helper()
		send(fmt.Sprintf(`{"id":%d,"method":"turn/start","params":{"threadId":%q,"input":[{"type":"text","text":%q}]}}`, id, thread, text))
		response(id)
		line := wait(func(line string) bool { return ojson.Get(line, "method").String() == "turn/completed" })
		if status := ojson.Get(line, "params.turn.status").String(); status != "completed" {
			t.Fatal(line)
		}
	}
	turn(3, "Remember drain-marker-7294.")
	if s.drain.Status().WebsocketSessions == 0 {
		t.Fatal("Codex did not use a persistent WebSocket")
	}
	instance := s.drain.Status().InstanceID
	if code, _ := drainCall(t, ts, http.MethodPost, drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	deadline := time.After(5 * time.Second)
	for !s.drain.Status().Ready {
		select {
		case <-deadline:
			t.Fatal("drain did not finish")
		default:
			drainCall(t, ts, "GET", drainPath, "", "")
		}
	}
	if code, _ := drainCall(t, ts, http.MethodDelete, drainPath, instance, testDrainOwner); code != 200 {
		t.Fatal(code)
	}
	turn(4, "What marker did I ask you to remember?")
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.payloads) < 2 || !strings.Contains(executor.payloads[len(executor.payloads)-1], "drain-marker-7294") {
		t.Fatal("Codex did not replay full conversation after maintenance reconnect")
	}
	t.Log("actual Codex app-server: persistent WebSocket, 1012 drain, next turn replays prior conversation")
}
