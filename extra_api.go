package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ExtraRequest struct {
	Method      string `json:"method"`
	Game        string `json:"game"`
	Market      string `json:"market"`
	Item        string `json:"item"`
	Days        int    `json:"days"`
	CompareJSON string `json:"compare_json"`
}

func extraSpec(x ExtraRequest) (url, verb, body string, err error) {
	if x.Game != "CsGo" && x.Game != "Dota2" {
		return "", "", "", errors.New("для API выбери CS2 или Dota2")
	}
	if x.Market != "Tm" && x.Market != "Buff" {
		return "", "", "", errors.New("выбери BUFF или Market")
	}
	marketCur := "RUB"
	if x.Market == "Buff" {
		marketCur = "CNY"
	}
	base := pulseHost
	switch x.Method {
	case "market_info":
		return pulseMarketURL(x.Game), "GET", "", nil
	case "supported_markets":
		return base + pulseSupportedMarketsPath, "GET", "", nil
	case "commissions":
		return base + pulseCommissionsPath, "GET", "", nil
	case "method_costs":
		return base + pulseMethodCostsPath, "GET", "", nil
	case "currencies":
		return base + pulseCurrenciesPath, "GET", "", nil
	case "advanced", "history", "best_offers":
		item := strings.TrimSpace(x.Item)
		if len(item) < 1 || len(item) > 300 {
			return "", "", "", errors.New("выбери предмет из списка после сканирования")
		}
		var url string
		var req any
		switch x.Method {
		case "advanced":
			url = base + pulseAdvancedInfoPath
			req = map[string]any{"market": x.Market, "game": x.Game, "marketHashNames": []string{item}, "currency": marketCur}
		case "history":
			url = base + pulseHistoryPath
			req = map[string]any{"market": x.Market, "game": x.Game, "marketHashNames": []string{item}, "currency": marketCur}
		case "best_offers":
			if x.Days < 1 || x.Days > 365 {
				return "", "", "", errors.New("период от 1 до 365 дней")
			}
			url = base + pulseCompareBestPath
			req = map[string]any{"game": x.Game, "marketHashNames": []string{item}, "daysCount": x.Days}
		}
		b, _ := json.Marshal(req)
		return url, "POST", string(b), nil
	case "compare_tables":
		if len(x.CompareJSON) < 2 || len(x.CompareJSON) > 12000 {
			return "", "", "", errors.New("заполни JSON параметров Compare Tables")
		}
		var req map[string]any
		if json.Unmarshal([]byte(x.CompareJSON), &req) != nil {
			return "", "", "", errors.New("некорректный JSON")
		}
		if req["game"] != x.Game || req["firstMarket"] != "Buff" || req["secondMarket"] != "Tm" {
			return "", "", "", errors.New("Compare Tables: укажи game и пару firstMarket=Buff, secondMarket=Tm")
		}
		if _, ok := req["firstMarketOptions"].(map[string]any); !ok {
			return "", "", "", errors.New("нет firstMarketOptions")
		}
		if _, ok := req["secondMarketOptions"].(map[string]any); !ok {
			return "", "", "", errors.New("нет secondMarketOptions")
		}
		if _, ok := req["paginationRequest"].(map[string]any); !ok {
			return "", "", "", errors.New("нет paginationRequest")
		}
		// Never allow input to override host or authorization headers.
		return base + pulseCompareTablesPath, "POST", x.CompareJSON, nil
	default:
		return "", "", "", errors.New("неизвестный метод Pulse")
	}
}

func (a *App) extraHandler(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var x ExtraRequest
	if !requestJSON(w, r, &x) {
		return
	}
	apiURL, method, body, err := extraSpec(x)
	if err != nil {
		jsonResponse(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.mu.Lock()
	key := a.apiKey
	a.mu.Unlock()
	if key == "" {
		jsonResponse(w, 400, map[string]string{"error": "сначала сохрани API-ключ Pulse"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	result, err := a.getURL(ctx, apiURL, method, body, "Api-Key", "", key)
	if err != nil {
		jsonResponse(w, 502, map[string]string{"error": fmt.Sprintf("%s: %v", x.Method, err)})
		return
	}
	jsonResponse(w, 200, map[string]any{"method": x.Method, "source": apiURL, "data": result})
}
