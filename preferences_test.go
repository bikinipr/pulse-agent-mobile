package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrefsPersistenceAndAllSettingsRoute(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := newApp()
	mux := http.NewServeMux()
	a.routes(mux)
	p := defaultPrefs()
	p.RememberAPI = false
	p.Game = "Dota2"
	p.Pages = 3
	p.PageSize = 250
	p.ExtraGame = "Dota2"
	p.ExtraMarket = "Buff"
	p.ExtraItem = "Demo item"
	p.ExcludeCase = true
	s := defaults()
	s.CNYRub = 14.19
	s.MinROI = 12
	s.ItemType = "Rifle"
	s.Blacklist = "Case"
	raw, _ := json.Marshal(map[string]any{"settings": s, "prefs": p, "api_key": "SAMPLE_DO_NOT_STORE"})
	req := httptest.NewRequest("POST", "http://localhost/api/all-settings", bytes.NewReader(raw))
	req.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(mustRead(t, prefsFile())), "SAMPLE_DO_NOT_STORE") {
		t.Fatal("secret leaked in prefs")
	}
	if strings.Contains(string(mustRead(t, settingsFile())), "SAMPLE_DO_NOT_STORE") {
		t.Fatal("secret leaked in settings")
	}
	b := newApp()
	if b.cfg.CNYRub != 14.19 || b.cfg.ItemType != "Rifle" || b.prefs.Game != "Dota2" || b.prefs.Pages != 3 || b.prefs.ExcludeCase != true {
		t.Fatalf("settings not reloaded: %+v %+v", b.cfg, b.prefs)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/all-settings", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SAMPLE_DO_NOT_STORE") {
		t.Fatalf("same-process refresh loses key: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(settingsFile()), "pulse_api.bin")); !os.IsNotExist(err) {
		t.Fatalf("plain or encrypted key persisted when remember=false: %v", err)
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestExtraAPIRequestPaths(t *testing.T) {
	cases := []struct{ name, method, path string }{
		{"base meta", "market_info", pulseMarketInfoPath},
		{"advanced", "advanced", pulseAdvancedInfoPath},
		{"history", "history", pulseHistoryPath},
		{"best offers", "best_offers", pulseCompareBestPath},
		{"markets", "supported_markets", pulseSupportedMarketsPath},
		{"commission", "commissions", pulseCommissionsPath},
		{"token costs", "method_costs", pulseMethodCostsPath},
		{"currencies", "currencies", pulseCurrenciesPath},
	}
	for _, tc := range cases {
		u, method, body, err := extraSpec(ExtraRequest{Method: tc.method, Game: "CsGo", Market: "Tm", Item: "Demo", Days: 30})
		if err != nil || !strings.Contains(u, tc.path) || !strings.HasPrefix(u, "https://api-pulse.tradeon.space/") {
			t.Fatalf("%s: %q %q %q err=%v", tc.name, u, method, body, err)
		}
		if tc.method == "advanced" && !strings.Contains(body, `"marketHashNames":["Demo"]`) {
			t.Fatalf("wrong body %s", body)
		}
	}
}
