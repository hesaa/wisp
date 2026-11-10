package main

import (
	"encoding/json"
	"os"
	"testing"

	_ "github.com/lxn/walk"
	_ "go.mau.fi/whatsmeow"
	_ "modernc.org/sqlite"
)

func TestManifestAndResources(t *testing.T) {
	raw, err := os.ReadFile("winres/winres.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Manifest map[string]map[string]struct {
			DPI string `json:"dpi-awareness"`
			CC6 bool   `json:"use-common-controls-v6"`
		} `json:"RT_MANIFEST"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	m := cfg.Manifest["#1"]["0409"]
	if !m.CC6 || m.DPI != "per monitor v2" {
		t.Fatalf("manifest must enable comctl v6 and per-monitor-v2 DPI, got %+v", m)
	}
	if _, err := os.Stat("rsrc_windows_amd64.syso"); err != nil {
		t.Fatal(err)
	}
}
