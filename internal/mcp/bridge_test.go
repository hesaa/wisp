package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBridge_StartWritesInfoAndAuthenticates(t *testing.T) {
	st := &fakeStore{}
	wa := &fakeSender{accounts: []string{"personal", "work"}}
	svc := NewService(st, wa)

	infoPath := filepath.Join(t.TempDir(), "Wisp", "mcp.json")
	b := NewBridge(svc, infoPath)
	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	rawInfo, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", infoPath, err)
	}
	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rawInfo, &info); err != nil {
		t.Fatalf("Unmarshal mcp.json: %v", err)
	}
	if info.Port <= 0 {
		t.Fatalf("expected positive port, got %d", info.Port)
	}
	if len(info.Token) != 64 {
		t.Fatalf("expected 64-char hex token, got %q (len=%d)", info.Token, len(info.Token))
	}

	rpcURL := "http://127.0.0.1:" + strconv.Itoa(info.Port) + "/rpc"

	// Unauthenticated request must return 401.
	unauthReq, _ := http.NewRequest(http.MethodPost, rpcURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	unauthResp, err := http.DefaultClient.Do(unauthReq)
	if err != nil {
		t.Fatalf("unauthenticated POST: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", unauthResp.StatusCode)
	}

	// Wrong token must also return 401.
	badTokReq, _ := http.NewRequest(http.MethodPost, rpcURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	badTokReq.Header.Set("Authorization", "Bearer wrong-token")
	badTokResp, err := http.DefaultClient.Do(badTokReq)
	if err != nil {
		t.Fatalf("bad token POST: %v", err)
	}
	_ = badTokResp.Body.Close()
	if badTokResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for wrong token, got %d", badTokResp.StatusCode)
	}

	// Authenticated tools/list succeeds.
	postRPC := func(payload string) map[string]any {
		req, err := http.NewRequest(http.MethodPost, rpcURL, bytes.NewBufferString(payload))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+info.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("Decode response: %v", err)
		}
		return out
	}

	listOut := postRPC(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if listOut["error"] != nil {
		t.Fatalf("unexpected tools/list error: %v", listOut["error"])
	}
	listResult, ok := listOut["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", listOut["result"])
	}
	tools, ok := listResult["tools"].([]any)
	if !ok || len(tools) != len(svc.Tools()) {
		t.Fatalf("expected %d tools, got %v", len(svc.Tools()), listResult["tools"])
	}

	// Authenticated tools/call succeeds.
	callOut := postRPC(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_accounts","arguments":{}}}`)
	if callOut["error"] != nil {
		t.Fatalf("unexpected tools/call error: %v", callOut["error"])
	}
	callRaw, _ := json.Marshal(callOut["result"])
	var toolRes ToolResult
	if err := json.Unmarshal(callRaw, &toolRes); err != nil {
		t.Fatalf("unmarshal ToolResult: %v", err)
	}
	if toolRes.IsError || len(toolRes.Content) != 1 {
		t.Fatalf("unexpected ToolResult: %+v", toolRes)
	}
	if !strings.Contains(toolRes.Content[0].Text, `"personal"`) || !strings.Contains(toolRes.Content[0].Text, `"work"`) {
		t.Fatalf("expected accounts in tools/call result, got %s", toolRes.Content[0].Text)
	}

	// Stop removes mcp.json.
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatalf("expected %q to be removed after Stop, stat err=%v", infoPath, err)
	}
}

func TestDefaultInfoPathAndSnippet(t *testing.T) {
	p, err := DefaultInfoPath()
	if err != nil {
		t.Fatalf("DefaultInfoPath: %v", err)
	}
	if !strings.HasSuffix(filepath.Clean(p), filepath.Join("Wisp", "mcp.json")) {
		t.Fatalf("unexpected DefaultInfoPath %q", p)
	}

	snippet := DefaultClientConfigSnippet(`C:\Program Files\Wisp\wisp-mcp.exe`)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(snippet), &parsed); err != nil {
		t.Fatalf("snippet is not valid JSON: %v\n%s", err, snippet)
	}
	if !strings.Contains(snippet, "wisp-mcp.exe") {
		t.Fatalf("expected exe path in snippet, got %s", snippet)
	}
}
