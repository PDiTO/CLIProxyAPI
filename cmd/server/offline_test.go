package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestOfflineCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	handled, code := runOfflineCommand([]string{"--capabilities"}, &out, &errOut)
	if !handled || code != 0 || !ojson.Valid(out.String()) || !strings.Contains(out.String(), "soonest-reset-first") {
		t.Fatalf("capabilities: handled=%v code=%d out=%q err=%q", handled, code, out.String(), errOut.String())
	}
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"legacy", "port: 8317\nrouting: {strategy: soonest-reset-first}\nremote-management: {secret-key: keep-me-private}\n", true},
		{"v8", "server: {port: 8317}\nrouting: {strategy: soonest-reset-first, session-affinity: true}\n", true},
		{"unknown", "routing: {strategy: definitely-invalid}\n", false},
		{"invalid-yaml", "routing: [private-secret", false},
		{"bad-type", "port: private-secret\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			errOut.Reset()
			handled, code := runOfflineCommand([]string{"-check-config", "-config", path}, &out, &errOut)
			if !handled || (code == 0) != tc.valid {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tc.data {
				t.Fatal("offline check rewrote config")
			}
			if strings.Contains(out.String()+errOut.String(), "private-secret") || strings.Contains(out.String()+errOut.String(), "keep-me-private") {
				t.Fatal("offline check leaked config content")
			}
		})
	}
}
