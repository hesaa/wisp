package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"

	"wa-go/internal/mcp"
	"wa-go/internal/store"
	"wa-go/internal/ui"
)

type mcpStubBackend struct {
	Backend
	accounts []string
}

func (b *mcpStubBackend) Accounts() []string { return b.accounts }

func (b *mcpStubBackend) SendText(context.Context, string, string, string, *store.Message, []string) (store.Message, error) {
	return store.Message{}, nil
}

func (b *mcpStubBackend) SendMedia(context.Context, string, string, string, string, *store.Message) (store.Message, error) {
	return store.Message{}, nil
}

func TestAppMCPBridge_LifecycleAndAppLockIndependence(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "wa.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	infoPath := filepath.Join(t.TempDir(), "Wisp", "mcp.json")
	a := &App{
		db:          db,
		mgr:         &mcpStubBackend{accounts: []string{acctA}},
		cfg:         map[string]string{},
		mcpInfoPath: infoPath,
		presence: presenceCtl{
			window:        func() (bool, bool) { return true, false },
			appearOffline: func() bool { return false },
			send:          func(types.Presence) {},
		},
	}
	t.Cleanup(func() { a.stopMCPBridge() })

	// Default ("false"): bridge must not start.
	a.syncMCPBridge()
	if a.mcpBridge != nil {
		t.Fatal("mcpBridge started when disabled by default")
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatalf("info file %q exists while MCP disabled: %v", infoPath, err)
	}

	// Enable via settingChanged: bridge starts and writes mcp.json.
	if err := db.SetSetting("", ui.KeyMCPEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	a.settingChanged(ui.KeyMCPEnabled)
	if a.mcpBridge == nil {
		t.Fatal("mcpBridge did not start after enabling KeyMCPEnabled")
	}
	firstBridge := a.mcpBridge

	// Calling syncMCPBridge again while enabled is idempotent.
	a.syncMCPBridge()
	if a.mcpBridge != firstBridge {
		t.Fatal("syncMCPBridge replaced an already-running bridge")
	}

	rawReq := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_accounts","arguments":{}}}`
	callListAccounts := func() {
		t.Helper()
		raw, err := os.ReadFile(infoPath)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", infoPath, err)
		}
		var info mcp.BridgeInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/rpc", info.Port), strings.NewReader(rawReq))
		req.Header.Set("Authorization", "Bearer "+info.Token)
		req.Header.Set("Content-Type", "application/json")
		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("list_accounts request failed: %v", err)
		}
		defer httpResp.Body.Close()
		var out mcp.RPCResponse
		if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if out.Error != nil {
			t.Fatalf("RPC error: %+v", out.Error)
		}
		b, _ := json.Marshal(out.Result)
		if !strings.Contains(string(b), acctA) {
			t.Fatalf("list_accounts result %s missing %q", b, acctA)
		}
	}
	callListAccounts()

	// App lock active must NOT stop or block the MCP bridge.
	a.locked = true
	a.presence.appLock(true)
	if a.mcpBridge == nil {
		t.Fatal("appLock(true) cleared mcpBridge")
	}
	callListAccounts()

	a.locked = false
	a.presence.appLock(false)
	if a.mcpBridge == nil {
		t.Fatal("appLock(false) cleared mcpBridge")
	}
	callListAccounts()

	// Disable via settingChanged: bridge stops and removes mcp.json.
	if err := db.SetSetting("", ui.KeyMCPEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	a.settingChanged(ui.KeyMCPEnabled)
	if a.mcpBridge != nil {
		t.Fatal("mcpBridge still set after disabling KeyMCPEnabled")
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatalf("info file %q still present after disable: %v", infoPath, err)
	}

	// Re-enable and verify stopMCPBridge() tears it down cleanly.
	if err := db.SetSetting("", ui.KeyMCPEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	a.settingChanged(ui.KeyMCPEnabled)
	if a.mcpBridge == nil {
		t.Fatal("mcpBridge failed to restart")
	}

	// Transient ui.Setting error must preserve the running bridge.
	db.Close()
	a.syncMCPBridge()
	if a.mcpBridge == nil {
		t.Fatal("syncMCPBridge stopped running bridge on ui.Setting error")
	}
	if _, err := os.Stat(infoPath); err != nil {
		t.Fatalf("info file %q missing after ui.Setting error: %v", infoPath, err)
	}

	a.stopMCPBridge()
	if a.mcpBridge != nil {
		t.Fatal("mcpBridge still set after stopMCPBridge")
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatalf("info file %q still present after stopMCPBridge: %v", infoPath, err)
	}
}
