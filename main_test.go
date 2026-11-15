package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func demoRequest() ScanRequest {
	stamp := time.Now().UTC().Format(time.RFC3339)
	base := map[string]any{"data": map[string]any{"items": []any{map[string]any{"id": "123", "name": "DEMO", "game": "cs2"}}}}
	market := map[string]any{"result": map[string]any{"records": []any{map[string]any{"id": "123", "buffCny": 90, "saleRub": 1600, "sales30": 40, "updated": stamp, "buffLink": "https://buff.163.com/"}}}}
	b, _ := json.Marshal(base)
	m, _ := json.Marshal(market)
	return ScanRequest{Base: b, Market: m, Mapping: Mapping{BasePath: "data.items", MarketPath: "result.records", BaseID: "id", MarketID: "id", Name: "base:name", Game: "base:game", Buff: "market:buffCny", Sale: "market:saleRub", Sales30: "market:sales30", Updated: "market:updated", BuffURL: "market:buffLink"}}
}
func TestDemoFinance(t *testing.T) {
	a := newApp()
	res, err := a.scan(context.Background(), demoRequest(), defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 1 {
		t.Fatalf("wanted one selected got %+v", res)
	}
	c := res.Selected[0]
	if math.Abs(c.Profit-267.7) > .0001 {
		t.Fatalf("wrong profit: %v", c.Profit)
	}
	if c.Cost != 1176.3 || c.Sales30 != 40 || c.ROI < 10 {
		t.Fatalf("wrong data: %+v", c)
	}
}
func Test39SalesReject(t *testing.T) {
	a := newApp()
	r := demoRequest()
	var m map[string]any
	_ = json.Unmarshal(r.Market, &m)
	rec := m["result"].(map[string]any)["records"].([]any)[0].(map[string]any)
	rec["sales30"] = 39
	r.Market, _ = json.Marshal(m)
	res, err := a.scan(context.Background(), r, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 0 || len(res.All) != 0 || res.Excluded != 1 {
		t.Fatalf("39 sales should reject: %+v", res)
	}
}
func TestMin500Reject(t *testing.T) {
	a := newApp()
	r := demoRequest()
	var m map[string]any
	_ = json.Unmarshal(r.Market, &m)
	rec := m["result"].(map[string]any)["records"].([]any)[0].(map[string]any)
	rec["buffCny"] = 20
	r.Market, _ = json.Marshal(m)
	res, err := a.scan(context.Background(), r, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 0 || len(res.All) != 0 || res.Excluded != 1 {
		t.Fatalf("cost under 500 should reject")
	}
}
func TestDuplicateID(t *testing.T) {
	a := newApp()
	r := demoRequest()
	var m map[string]any
	_ = json.Unmarshal(r.Market, &m)
	rows := m["result"].(map[string]any)["records"].([]any)
	m["result"].(map[string]any)["records"] = append(rows, rows[0])
	r.Market, _ = json.Marshal(m)
	_, err := a.scan(context.Background(), r, defaults())
	if err == nil {
		t.Fatal("duplicate MarketInfo IDs must error")
	}
}
func TestURLAllowlist(t *testing.T) {
	if safeLink("https://evil.com/") != "" {
		t.Fatal("nonallowlisted link")
	}
	if safeLink("javascript:alert(1)") != "" {
		t.Fatal("XSS link")
	}
}
func TestConfig(t *testing.T) {
	if defaults().valid() != nil {
		t.Fatal("bad defaults")
	}
	s := defaults()
	s.CNYRub = -1
	if s.valid() == nil {
		t.Fatal("negative course must reject")
	}
}

func TestAPIKeyFieldIsVisibleAndNotSaved(t *testing.T) {
	b, err := assets.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Count(html, `id="api_key"`) != 1 {
		t.Fatal("expected exactly one API key field")
	}
	if !strings.Contains(html, `id="api_key" type="password" autocomplete="off"`) {
		t.Fatal("key must be hidden by default with autocomplete off")
	}
	if !strings.Contains(html, `id="show_key" onclick="toggleAPIKey()"`) {
		t.Fatal("missing show/hide button")
	}
	keyAt := strings.Index(html, `id="api_key"`)
	if keyAt < 0 || !strings.Contains(html, "https://api-pulse.tradeon.space/public-api/item/base-info") || !strings.Contains(html, "https://api-pulse.tradeon.space/public-api/item/market-info") {
		t.Fatal("API field and fixed URLs must be present")
	}
	if strings.Contains(string(mustSettingsJSON(t)), "api_key") || strings.Contains(string(mustSettingsJSON(t)), `"key"`) {
		t.Fatal("settings must not contain API key")
	}
}
func mustSettingsJSON(t *testing.T) []byte {
	t.Helper()
	b, e := json.Marshal(defaults())
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestUIOnlyQualifiedItemsAndBuffLink(t *testing.T) {
	b, err := assets.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, required := range []string{"Скины, прошедшие фильтры", "Открыть BUFF", "item_search", "lastData.selected", "resolveBuffLink", "resolveMarketLink", "Скачать отобранные предметы CSV"} {
		if !strings.Contains(html, required) {
			t.Fatalf("UI missing: %s", required)
		}
	}
	for _, removed := range []string{`id="tab_all"`, `id="tab_selected"`, `id="rejected"`, `Все найденные BUFF`} {
		if strings.Contains(html, removed) {
			t.Fatalf("UI still exposes rejects: %s", removed)
		}
	}
}

func TestCSVFiltersRejectedEvenWithAllViewParam(t *testing.T) {
	app := newApp()
	app.latest = ScanResult{
		All:      []Candidate{{Name: "Negative Position", Game: "cs2", Status: "Не подходит", Profit: -100, BuffURL: "https://buff.163.com/goods/200"}},
		Selected: []Candidate{{Name: "Profitable Skin", Game: "cs2", Status: "Подходит", Profit: 220, ROI: 14, BuffURL: "https://buff.163.com/goods/1"}},
	}
	mux := http.NewServeMux()
	app.routes(mux)
	for _, path := range []string{"/api/csv", "/api/csv?view=all"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("HTTP %d", rec.Code)
		}
		text := rec.Body.String()
		if !strings.Contains(text, "Profitable Skin") || !strings.Contains(text, "https://buff.163.com/goods/1") || strings.Contains(text, "Negative Position") {
			t.Fatalf("CSV leaked excluded item: %s", text)
		}
	}
}

func TestNegativeProfitExcludedEvenIfMinROIIsZero(t *testing.T) {
	a := newApp()
	r := demoRequest()
	var m map[string]any
	_ = json.Unmarshal(r.Market, &m)
	rec := m["result"].(map[string]any)["records"].([]any)[0].(map[string]any)
	rec["saleRub"] = 1100 // < 1176.3 RUB cost / .9025 = negative net P&L
	r.Market, _ = json.Marshal(m)
	cfg := defaults()
	cfg.MinROI = 0
	res, err := a.scan(context.Background(), r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.All) != 0 || len(res.Selected) != 0 || res.Excluded != 1 {
		t.Fatalf("negative-profit item must not be visible: %+v", res)
	}
}
