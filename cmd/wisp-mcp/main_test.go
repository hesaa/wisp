package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wa-go/internal/mcp"
	"wa-go/internal/store"
)

type stubStore struct{}

func (stubStore) ListChats(string) ([]store.Chat, error)               { return nil, nil }
func (stubStore) People(string) ([]store.Person, error)                { return nil, nil }
func (stubStore) GetChatMessages(string, string, int) ([]store.Message, error) { return nil, nil }

type stubSender struct {
	accounts []string
}

func (s stubSender) Accounts() []string { return s.accounts }
func (stubSender) SendText(context.Context, string, string, string, *store.Message, []string) (store.Message, error) {
	return store.Message{}, nil
}
func (stubSender) SendMedia(context.Context, string, string, string, string, *store.Message) (store.Message, error) {
	return store.Message{}, nil
}

func decodeLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestStdio_InitializeAndToolsList(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	infoPath := filepath.Join(t.TempDir(), "missing-mcp.json")
	if err := serveStdio(strings.NewReader(input), &out, infoPath); err != nil {
		t.Fatalf("serveStdio: %v", err)
	}

	msgs := decodeLines(t, out.String())
	if len(msgs) != 2 {
		t.Fatalf("expected 2 responses (notifications/initialized has none), got %d: %s", len(msgs), out.String())
	}

	initResp := msgs[0]
	if initResp["id"] != float64(1) {
		t.Fatalf("expected id=1, got %v", initResp["id"])
	}
	initResult, ok := initResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected initialize result object, got %T", initResp["result"])
	}
	if initResult["protocolVersion"] == "" {
		t.Fatalf("expected protocolVersion in initialize result")
	}
	if _, ok := initResult["capabilities"].(map[string]any); !ok {
		t.Fatalf("expected capabilities in initialize result")
	}

	listResp := msgs[1]
	if listResp["id"] != float64(2) {
		t.Fatalf("expected id=2, got %v", listResp["id"])
	}
	listResult, ok := listResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected tools/list result object, got %T", listResp["result"])
	}
	tools, ok := listResult["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("expected non-empty tools list, got %v", listResult["tools"])
	}
}

func TestStdio_ToolsCallWhenWispOffline(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_accounts","arguments":{}}}` + "\n"

	var out bytes.Buffer
	infoPath := filepath.Join(t.TempDir(), "nonexistent", "mcp.json")
	if err := serveStdio(strings.NewReader(input), &out, infoPath); err != nil {
		t.Fatalf("serveStdio: %v", err)
	}

	msgs := decodeLines(t, out.String())
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	resp := msgs[0]
	if resp["error"] != nil {
		t.Fatalf("expected tool result with isError=true, not JSON-RPC error: %v", resp["error"])
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", resp["result"])
	}
	if res["isError"] != true {
		t.Fatalf("expected isError=true, got %v", res["isError"])
	}
	rawRes, _ := json.Marshal(res)
	if !strings.Contains(string(rawRes), "Wisp is not running or MCP is disabled") {
		t.Fatalf("expected offline message, got %s", rawRes)
	}
}

func TestStdio_ToolsCallForwardsToBridge(t *testing.T) {
	svc := mcp.NewService(stubStore{}, stubSender{accounts: []string{"acct-main", "acct-biz"}})
	infoPath := filepath.Join(t.TempDir(), "Wisp", "mcp.json")
	b := mcp.NewBridge(svc, infoPath)
	if err := b.Start(); err != nil {
		t.Fatalf("Bridge.Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	input := `{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"list_accounts","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if err := serveStdio(strings.NewReader(input), &out, infoPath); err != nil {
		t.Fatalf("serveStdio: %v", err)
	}

	msgs := decodeLines(t, out.String())
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	resp := msgs[0]
	if resp["id"] != "call-1" {
		t.Fatalf("expected id='call-1', got %v", resp["id"])
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	rawRes, _ := json.Marshal(resp["result"])
	if !strings.Contains(string(rawRes), "acct-main") || !strings.Contains(string(rawRes), "acct-biz") {
		t.Fatalf("expected bridge accounts in result, got %s", rawRes)
	}
}

func writeTestBridgeInfo(t *testing.T, port int, token string) string {
	t.Helper()
	infoPath := filepath.Join(t.TempDir(), "mcp.json")
	b, err := json.Marshal(mcp.BridgeInfo{Port: port, Token: token})
	if err != nil {
		t.Fatalf("marshal bridge info: %v", err)
	}
	if err := os.WriteFile(infoPath, b, 0o600); err != nil {
		t.Fatalf("write bridge info: %v", err)
	}
	return infoPath
}

func TestStdio_BridgeTimeoutsAndNonOfflineErrors(t *testing.T) {
	if bridgeClient.Timeout != 0 {
		t.Fatalf("bridgeClient.Timeout = %v, want 0 (per-request context controls timeout)", bridgeClient.Timeout)
	}
	if toolsListTimeout != 5*time.Second {
		t.Fatalf("toolsListTimeout = %v, want 5s", toolsListTimeout)
	}
	if toolsCallTimeout != 5*time.Minute {
		t.Fatalf("toolsCallTimeout = %v, want 5m", toolsCallTimeout)
	}

	callInput := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_accounts","arguments":{}}}` + "\n"

	callText := func(t *testing.T, infoPath string) string {
		t.Helper()
		var out bytes.Buffer
		if err := serveStdio(strings.NewReader(callInput), &out, infoPath); err != nil {
			t.Fatalf("serveStdio: %v", err)
		}
		msgs := decodeLines(t, out.String())
		if len(msgs) != 1 {
			t.Fatalf("expected 1 response, got %d", len(msgs))
		}
		res, ok := msgs[0]["result"].(map[string]any)
		if !ok || res["isError"] != true {
			t.Fatalf("expected tool error result, got %+v", msgs[0])
		}
		content, _ := res["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("expected 1 content item, got %+v", res["content"])
		}
		item, _ := content[0].(map[string]any)
		text, _ := item["text"].(string)
		return text
	}

	t.Run("timeout", func(t *testing.T) {
		origTimeout := toolsCallTimeout
		toolsCallTimeout = 40 * time.Millisecond
		t.Cleanup(func() { toolsCallTimeout = origTimeout })

		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}))
		t.Cleanup(func() {
			close(done)
			srv.Close()
		})

		port := srv.Listener.Addr().(*net.TCPAddr).Port
		infoPath := writeTestBridgeInfo(t, port, "tok")
		got := callText(t, infoPath)
		if got != bridgeTimeoutMsg {
			t.Fatalf("got %q, want %q", got, bridgeTimeoutMsg)
		}
	})

	t.Run("http_500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		port := srv.Listener.Addr().(*net.TCPAddr).Port
		infoPath := writeTestBridgeInfo(t, port, "tok")
		got := callText(t, infoPath)
		if !strings.HasPrefix(got, "MCP bridge error: ") || !strings.Contains(got, "500") {
			t.Fatalf("expected MCP bridge error with status 500, got %q", got)
		}
	})

	t.Run("decode_error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{not-json"))
		}))
		t.Cleanup(srv.Close)

		port := srv.Listener.Addr().(*net.TCPAddr).Port
		infoPath := writeTestBridgeInfo(t, port, "tok")
		got := callText(t, infoPath)
		if !strings.HasPrefix(got, "MCP bridge error: ") {
			t.Fatalf("expected MCP bridge error on invalid JSON, got %q", got)
		}
	})

	t.Run("dial_error_reports_offline", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		infoPath := writeTestBridgeInfo(t, port, "tok")
		got := callText(t, infoPath)
		if got != wispOfflineMessage {
			t.Fatalf("got %q, want %q", got, wispOfflineMessage)
		}
	})
}
