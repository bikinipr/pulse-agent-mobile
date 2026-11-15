package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFix093ProfitableItemSurvivesMissingLinksAndOldQuotes(t *testing.T) {
	old := time.Now().UTC().Add(-9 * time.Hour).Format(time.RFC3339)
	buff := map[string]any{"name": "Item A", "sellOffers": map[string]any{"bestOffer": 100}, "updateTime": old, "marketUrl": "https://buff.163.com/market/dota2"}
	tm := map[string]any{"name": "Item A", "sellOffers": map[string]any{"bestOffer": 2100}, "statisticsInfo": map[string]any{"averagePrice30Day": 2000, "sales30Day": 70}, "updateTime": old}
	c, ok := candidateFromBaseInfo("CsGo", buff, tm, defaults(), time.Now())
	if !ok {
		t.Fatalf("profitable candidate rejected due to links/stale time: %+v", c)
	}
	if c.Profit <= 0 || c.ROI < 10 || c.Sales30 < 40 || !strings.Contains(c.Risk, "время котировки") || !strings.Contains(c.Risk, "ссылки") {
		t.Fatalf("incomplete risk data: %+v", c)
	}
	r := ScanResult{Selected: []Candidate{c}, SeenCount: 1}
	r.finalizeVisible(defaults())
	if len(r.Selected) != 1 {
		t.Fatalf("candidate unexpectedly deleted by final filter %+v", r)
	}
}

func TestFix093CaseWhitespaceAndDuplicatedNames(t *testing.T) {
	m1 := map[string]any{"name": " Item   A ", "updateTime": "2026-11-01T00:00:00Z"}
	m2 := map[string]any{"name": "item\u00a0a", "updateTime": "2026-11-02T00:00:00Z"}
	idx, dup := pulseMarketRecord([]any{m1, m2})
	if dup != 1 || len(idx) != 1 || idx["item a"] == nil {
		t.Fatalf("index failed: %v, %d", idx, dup)
	}
	if pulseItemKey("ITEM   A") != pulseItemKey("item\u00a0a") {
		t.Fatal("normalized match broken")
	}
}

func TestFix093DiagnosticStagesForThreeMockScans(t *testing.T) {
	// Repeat complete scan three times without external API calls or credits.
	for run := 0; run < 3; run++ {
		a := newApp()
		a.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			var body string
			if req.URL.Query().Get("Market") == "Buff" {
				body = `[{"name":"Item A","marketUrl":"","sellOffers":{"bestOffer":100}},
                      {"name":"Item B","sellOffers":{"bestOffer":100}},
                      {"name":"Item C","sellOffers":{"bestOffer":100}},
                      {"name":"Item D","sellOffers":{"bestOffer":120}}]`
			} else {
				body = `[{"name":"item  a","sellOffers":{"bestOffer":2000},"statisticsInfo":{"averagePrice30Day":2000,"sales30Day":70}},
                      {"name":"Item B","sellOffers":{"bestOffer":2000},"statisticsInfo":{"averagePrice30Day":2000,"sales30Day":20}},
                      {"name":"Item C","sellOffers":{"bestOffer":1100},"statisticsInfo":{"averagePrice30Day":1100,"sales30Day":80}},
                      {"name":"Item D","sellOffers":{"bestOffer":2000},"statisticsInfo":{"averagePrice30Day":2000,"sales30Day":80}}]`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
		})}
		res, e := a.scanPulse(context.Background(), ScanRequest{Mode: "pulse", API: APIOptions{Key: "TEST_ONLY"}, Pulse: PulseOptions{Game: "CsGo", Pages: 1, PageSize: 4}}, defaults())
		if e != nil {
			t.Fatal(e)
		}
		if len(res.Selected) != 2 || res.Matched != 4 || res.PriceReady != 4 || res.CostPassed != 4 || res.SalesPassed != 3 || res.ROIPassed != 2 {
			t.Fatalf("mock scan %d wrong stats: selected %d matched=%d price=%d cost=%d sales=%d roi=%d rejected=%+v", run+1, len(res.Selected), res.Matched, res.PriceReady, res.CostPassed, res.SalesPassed, res.ROIPassed, res.RejectStats)
		}
		for _, item := range res.Selected {
			if item.Name != "Item A" && item.Name != "Item D" {
				t.Fatal("ineligible item visible", item.Name)
			}
		}
	}
	t.Log(fmt.Sprint("3 повторных сканирования — без обращения к Pulse и без расхода токенов"))
}

func TestFix093HasDiagnosticUiElement(t *testing.T) {
	bytes, err := assets.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(bytes)
	for _, word := range []string{`id="reject_stats"`, `data.reject_stats`, `data.price_ready`, `data.roi_passed`} {
		if !strings.Contains(s, word) {
			t.Fatal("missing diagnostic element", word)
		}
	}
}

func TestFix093BudgetButtonIsLimited(t *testing.T) {
	b, err := assets.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, needle := range []string{`onclick="scanBudgetCheck()"`, `req.pulse.pages=1`, `req.pulse.page_size=100`, `req.pulse.include_market_info=false`, `req.verify_market=false`, `req.telegram.enabled=false`} {
		if !strings.Contains(s, needle) {
			t.Fatal("expected bounded test option", needle)
		}
	}
}
