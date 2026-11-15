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

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLivePulseOfficialEndpointsAndResult(t *testing.T) {
	a := newApp()
	calls := 0
	stamp := time.Now().UTC().Format(time.RFC3339)
	a.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Api-Key") != "TEST_DO_NOT_STORE" {
			t.Fatalf("missing official auth header")
		}
		if req.URL.Host != "api-pulse.tradeon.space" {
			t.Fatalf("wrong host %s", req.URL.Host)
		}
		var body string
		switch req.URL.Path {
		case pulseBaseInfoPath:
			q := req.URL.Query()
			if q.Get("Game") != "CsGo" || q.Get("TakeCount") != "1" || q.Get("SkipCount") != "0" {
				t.Fatalf("wrong pagination/game %v", q)
			}
			if q.Get("Market") == "Buff" && q.Get("Currency") == "CNY" {
				body = fmt.Sprintf(`[{"name":"Test official API format","marketUrl":"https://buff.163.com/goods/123","sellOffers":{"bestOffer":100},"statisticsInfo":{"sales30Day":50},"updateTime":%q}]`, stamp)
			} else if q.Get("Market") == "Tm" && q.Get("Currency") == "RUB" {
				body = fmt.Sprintf(`[{"name":"Test official API format","marketUrl":"https://market.csgo.com/item/123","sellOffers":{"bestOffer":2100},"statisticsInfo":{"averagePrice30Day":2000,"sales7Day":15,"sales30Day":50},"updateTime":%q}]`, stamp)
			} else {
				t.Fatalf("wrong market/currency %v", q)
			}
		case pulseMarketInfoPath:
			if req.URL.Query().Get("Game") != "CsGo" {
				t.Fatalf("market info game missing")
			}
			body = `[{"name":"Test official API format","marketInfos":[{"market":"Tm","itemMarketId":123}]}]`
		default:
			t.Fatalf("unexpected endpoint %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	res, err := a.scan(context.Background(), ScanRequest{Mode: "pulse", API: APIOptions{Key: "TEST_DO_NOT_STORE"}, Pulse: PulseOptions{Game: "CsGo", Pages: 1, PageSize: 1, IncludeMarketInfo: true}}, defaults())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 official calls, got %d", calls)
	}
	if res.Matched != 1 || len(res.Selected) != 1 {
		t.Fatalf("expected matching candidate: %+v", res)
	}
	c := res.Selected[0]
	if c.Cost != 1307 || c.Sale != 2000 || c.Profit != 498 || c.Sales30 != 50 {
		t.Fatalf("incorrect economics %+v", c)
	}
	if !strings.Contains(res.Notice, "MarketInfo записей: 1") {
		t.Fatalf("MarketInfo not counted: %s", res.Notice)
	}
}
func TestLiveRejectSparseSales(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	b := map[string]any{"name": "Test", "sellOffers": map[string]any{"bestOffer": 100}, "marketUrl": "https://buff.163.com/goods/1", "updateTime": now}
	m := map[string]any{"name": "Test", "sellOffers": map[string]any{"bestOffer": 2100}, "marketUrl": "https://market.csgo.com/item/1", "statisticsInfo": map[string]any{"averagePrice30Day": 2000, "sales30Day": 39}, "updateTime": now}
	c, ok := candidateFromBaseInfo("CsGo", b, m, defaults(), time.Now().UTC())
	if ok || !strings.Contains(c.Reason, "меньше необходимого числа продаж") {
		t.Fatalf("must reject 39 monthly sales: %+v", c)
	}
}

func TestLiveShowsOnlyEligibleBuffItemsWithLinks(t *testing.T) {
	a := newApp()
	stamp := time.Now().UTC().Format(time.RFC3339)
	a.client = &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		if req.URL.Query().Get("Market") == "Buff" {
			body = fmt.Sprintf(`[ 
            {"name":"Matched Skin","type":"Rifle","quality":"Classified","marketUrl":"https://buff.163.com/goods/100","sellOffers":{"bestOffer":100},"updateTime":%q},
            {"name":"Only on BUFF page","type":"Rifle","quality":"Classified","marketUrl":"https://buff.163.com/goods/200","sellOffers":{"bestOffer":70},"updateTime":%q},
            {"name":"Sparse sales","type":"Case","marketUrl":"https://buff.163.com/goods/300","sellOffers":{"bestOffer":90},"updateTime":%q},
            {"name":"Losing Skin","type":"Rifle","marketUrl":"https://buff.163.com/goods/400","sellOffers":{"bestOffer":150},"updateTime":%q}
            ]`, stamp, stamp, stamp, stamp)
		} else {
			body = fmt.Sprintf(`[ 
            {"name":"Matched Skin","marketUrl":"https://market.csgo.com/item/100","sellOffers":{"bestOffer":2200},"statisticsInfo":{"averagePrice30Day":2100,"sales30Day":60},"updateTime":%q},
            {"name":"Sparse sales","marketUrl":"https://market.csgo.com/item/300","sellOffers":{"bestOffer":1800},"statisticsInfo":{"averagePrice30Day":1750,"sales30Day":10},"updateTime":%q},
            {"name":"Losing Skin","marketUrl":"https://market.csgo.com/item/400","sellOffers":{"bestOffer":1200},"statisticsInfo":{"averagePrice30Day":1180,"sales30Day":120},"updateTime":%q}
            ]`, stamp, stamp, stamp)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
	cfg := defaults()
	cfg.MinROI = 0 // loss still MUST disappear
	res, err := a.scan(context.Background(), ScanRequest{Mode: "pulse", API: APIOptions{Key: "DUMMY"}, Pulse: PulseOptions{Game: "CsGo", Pages: 1, PageSize: 10}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.SeenCount != 4 || res.Matched != 3 || res.Excluded != 3 || len(res.Selected) != 1 || len(res.All) != 1 || len(res.Rejected) != 0 {
		t.Fatalf("must hide unmatched, low sales and loss: %+v", res)
	}
	if res.Selected[0].Name != "Matched Skin" || res.Selected[0].BuffURL != "https://buff.163.com/goods/100" {
		t.Fatalf("only matched profitable candidate with BUFF link should remain: %+v", res.Selected)
	}
	if len(res.ItemTypes) != 2 || len(res.Qualities) != 1 {
		t.Fatalf("types and qualities should remain available to dropdowns: %+v", res)
	}
}

func TestLinksFromCorrectMarketOnly(t *testing.T) {
	if buffLink("https://market.csgo.com/item/1") != "" || buffLink("https://steamcommunity.com/market") != "" {
		t.Fatal("not BUFF links")
	}
	if buffLink("https://buff.163.com/goods/111") != "https://buff.163.com/goods/111" {
		t.Fatal("valid BUFF")
	}
	if tmLink("https://buff.163.com/goods/111") != "" {
		t.Fatal("not TM link")
	}
}
