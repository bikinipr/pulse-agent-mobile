package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var assets embed.FS

const maxBody = 24 << 20

type Settings struct {
	CNYRub      float64 `json:"cny_rub"`
	Factor      float64 `json:"net_revenue_factor"`
	MinPurchase float64 `json:"min_purchase_rub"`
	MinROI      float64 `json:"min_roi_pct"`
	MinSales30  int     `json:"min_sales_30d"`
	MinSales7   int     `json:"min_sales_7d"`
	MaxPurchase float64 `json:"max_purchase_rub"`
	MaxROI      float64 `json:"max_roi_pct"`
	ItemType    string  `json:"item_type"`
	Quality     string  `json:"quality"`
	Blacklist   string  `json:"blacklist"`
	SaleModel   string  `json:"sale_price_model"`
	DiscountPct float64 `json:"sale_discount_pct"`
	PollMinutes int     `json:"poll_minutes"`
}

func defaults() Settings {
	return Settings{CNYRub: 13.07, Factor: .9025, MinPurchase: 500, MinROI: 10, MinSales30: 40, MinSales7: 0, SaleModel: "safe", DiscountPct: 1, PollMinutes: 30}
}
func (s Settings) valid() error {
	if !isFinite(s.CNYRub) || s.CNYRub <= 0 {
		return errors.New("курс должен быть положительным")
	}
	if !isFinite(s.Factor) || s.Factor <= 0 || s.Factor > 1 {
		return errors.New("коэффициент чистой выручки должен быть в интервале (0, 1]")
	}
	if !isFinite(s.MinPurchase) || s.MinPurchase < 0 || !isFinite(s.MinROI) || s.MinROI < 0 || s.MinSales30 < 0 || s.MinSales7 < 0 || !isFinite(s.MaxPurchase) || s.MaxPurchase < 0 || !isFinite(s.MaxROI) || s.MaxROI < 0 || !isFinite(s.DiscountPct) || s.DiscountPct < 0 || s.DiscountPct > 20 || (s.SaleModel != "safe" && s.SaleModel != "min" && s.SaleModel != "average") || s.PollMinutes < 1 {
		return errors.New("неверные значения фильтров")
	}
	return nil
}
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func settingsFile() string {
	dir, e := os.UserConfigDir()
	if e != nil {
		dir = "."
	}
	return filepath.Join(dir, "PulseArbitrageAgent", "settings.json")
}
func loadConfig() Settings {
	s := defaults()
	b, e := os.ReadFile(settingsFile())
	if e == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.SaleModel == "" { // v0.6 settings.json did not contain these fields
		s.SaleModel = "safe"
		s.DiscountPct = 1
	}
	if s.valid() != nil {
		return defaults()
	}
	return s
}
func saveConfig(s Settings) error {
	if e := s.valid(); e != nil {
		return e
	}
	path := settingsFile()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return atomicFile(path, b, 0600)
}

type Mapping struct {
	BasePath    string `json:"base_path"`
	MarketPath  string `json:"market_path"`
	BaseID      string `json:"base_id"`
	MarketID    string `json:"market_id"`
	Name        string `json:"name"`
	Game        string `json:"game"`
	DefaultGame string `json:"default_game"`
	Buff        string `json:"buff"`
	Sale        string `json:"sale"`
	Sales30     string `json:"sales30"`
	Updated     string `json:"updated"`
	BuffURL     string `json:"buff_url"`
	MarketURL   string `json:"market_url"`
}
type APIOptions struct {
	BaseURL      string `json:"base_url"`
	MarketURL    string `json:"market_url"`
	BaseMethod   string `json:"base_method"`
	MarketMethod string `json:"market_method"`
	BaseBody     string `json:"base_body"`
	MarketBody   string `json:"market_body"`
	Header       string `json:"header"`
	Prefix       string `json:"prefix"`
	Key          string `json:"key"`
}
type TelegramOptions struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}
type ScanRequest struct {
	Mode         string          `json:"mode"`
	Pulse        PulseOptions    `json:"pulse"`
	Base         json.RawMessage `json:"base"`
	Market       json.RawMessage `json:"market"`
	Mapping      Mapping         `json:"mapping"`
	API          APIOptions      `json:"api"`
	VerifyMarket bool            `json:"verify_market"`
	Telegram     TelegramOptions `json:"telegram"`
}
type Candidate struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Game          string  `json:"game"`
	BUFFCNY       float64 `json:"buff_cny"`
	Cost          float64 `json:"cost_rub"`
	Sale          float64 `json:"sale_rub"`
	Net           float64 `json:"net_rub"`
	Profit        float64 `json:"profit_rub"`
	ROI           float64 `json:"roi_pct"`
	Sales30       int     `json:"sales_30d"`
	Sales7        int     `json:"sales_7d"`
	MarketMin     float64 `json:"market_min_rub"`
	Average30     float64 `json:"average_30d_rub"`
	ItemType      string  `json:"item_type"`
	Quality       string  `json:"quality"`
	PriceMethod   string  `json:"price_method"`
	BuffURL       string  `json:"buff_url"`
	MarketURL     string  `json:"market_url"`
	QuoteTime     string  `json:"quote_time,omitempty"`
	Risk          string  `json:"risk"`
	Reason        string  `json:"reason,omitempty"`
	Status        string  `json:"status,omitempty"`
	ExpectedPrice float64 `json:"expected_sale_rub,omitempty"`
	Volatility    float64 `json:"daily_volatility_pct,omitempty"`
	StressProfit  float64 `json:"profit_after_5pct_drop_rub,omitempty"`
}
type ScanResult struct {
	BaseCount   int            `json:"base_rows"`
	MarketCount int            `json:"market_rows"`
	Matched     int            `json:"matched"`
	Unmatched   int            `json:"unmatched"`
	PriceReady  int            `json:"price_ready"`
	CostPassed  int            `json:"cost_passed"`
	SalesPassed int            `json:"sales_passed"`
	ROIPassed   int            `json:"roi_passed"`
	RejectStats map[string]int `json:"reject_stats"`
	Preliminary int            `json:"preliminary_count"`
	Verified    int            `json:"verified_count"`
	SeenCount   int            `json:"seen_count"`
	Excluded    int            `json:"excluded_count"`
	ItemTypes   []string       `json:"available_types"`
	Qualities   []string       `json:"available_qualities"`
	Selected    []Candidate    `json:"selected"`
	All         []Candidate    `json:"all"`
	Rejected    []Candidate    `json:"rejected"`
	BaseShape   any            `json:"base_shape"`
	MarketShape any            `json:"market_shape"`
	Notice      string         `json:"notice"`
	Time        string         `json:"time"`
}
type Status struct {
	Active    bool   `json:"active"`
	LastRun   string `json:"last_run"`
	LastError string `json:"last_error"`
	Selected  int    `json:"selected"`
	Running   bool   `json:"running"`
	NextRun   string `json:"next_run"`
}
type App struct {
	mu            sync.Mutex
	cfg           Settings
	prefs         UserPrefs
	apiKey        string
	tgToken       string
	latest        ScanResult
	status        Status
	activeRequest *ScanRequest
	lastNotified  map[string]alertStamp
	lastRun       time.Time
	running       bool
	client        *http.Client
}

type alertStamp struct {
	At  time.Time
	ROI float64
}

func newApp() *App {
	p := loadPrefs()
	apiKey, _ := loadSecret("pulse_api")
	tgToken, _ := loadSecret("telegram_bot")
	if cloudMode() {
		apiKey = os.Getenv("PULSE_API_KEY")
		tgToken = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	return &App{prefs: p, apiKey: apiKey, tgToken: tgToken, lastNotified: make(map[string]alertStamp), cfg: loadConfig(), client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}, latest: ScanResult{Selected: []Candidate{}, All: []Candidate{}, Rejected: []Candidate{}}}
}

func dotted(obj any, path string) any {
	if path == "" {
		return obj
	}
	for _, part := range strings.Split(path, ".") {
		switch x := obj.(type) {
		case map[string]any:
			var ok bool
			obj, ok = x[part]
			if !ok {
				return nil
			}
		case []any:
			i, e := strconv.Atoi(part)
			if e != nil || i < 0 || i >= len(x) {
				return nil
			}
			obj = x[i]
		default:
			return nil
		}
	}
	return obj
}
func findRows(obj any, depth int) ([]any, bool) {
	if v, ok := obj.([]any); ok {
		return v, true
	}
	if depth > 4 {
		return nil, false
	}
	if v, ok := obj.(map[string]any); ok {
		for _, k := range []string{"items", "data", "result", "results", "records", "list", "BaseInfos", "MarketInfo"} {
			if child, ok := v[k]; ok {
				if a, found := findRows(child, depth+1); found {
					return a, true
				}
			}
		}
	}
	return nil, false
}
func rows(obj any, path string) ([]any, error) {
	if path != "" {
		v := dotted(obj, path)
		a, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("поле %q не является массивом записей", path)
		}
		return a, nil
	}
	a, ok := findRows(obj, 0)
	if !ok {
		return nil, errors.New("массив записей не найден: укажи путь JSON")
	}
	return a, nil
}
func shape(v any, depth int) any {
	if depth >= 4 {
		return fmt.Sprintf("%T", v)
	}
	switch x := v.(type) {
	case map[string]any:
		r := map[string]any{}
		i := 0
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if i >= 60 {
				break
			}
			r[k] = shape(x[k], depth+1)
			i++
		}
		return r
	case []any:
		if len(x) == 0 {
			return "array(0)"
		}
		return map[string]any{"array_length": len(x), "first": shape(x[0], depth+1)}
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	}
	return "other"
}
func asText(v any) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
func asNumber(v any) (float64, bool) {
	if v == nil {
		return 0, false
	}
	s := strings.ReplaceAll(asText(v), ",", ".")
	n, e := strconv.ParseFloat(s, 64)
	return n, e == nil && isFinite(n)
}
func take(b, m map[string]any, ref string) any {
	p := strings.SplitN(ref, ":", 2)
	if len(p) != 2 {
		return nil
	}
	if p[0] == "base" {
		return dotted(b, p[1])
	}
	if p[0] == "market" {
		return dotted(m, p[1])
	}
	return nil
}
func safeLink(v any) string {
	u, e := url.Parse(asText(v))
	if e != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	switch u.Hostname() {
	case "buff.163.com", "market.csgo.com", "market.dota2.net", "pulse.tradeon.space", "steamcommunity.com":
		return u.String()
	}
	return ""
}
func buffLink(v any) string {
	u := safeLink(v)
	if u == "" {
		return ""
	}
	parsed, _ := url.Parse(u)
	if parsed.Hostname() != "buff.163.com" {
		return ""
	}
	return u
}
func tmLink(v any) string {
	u := safeLink(v)
	if u == "" {
		return ""
	}
	parsed, _ := url.Parse(u)
	if parsed.Hostname() != "market.csgo.com" && parsed.Hostname() != "market.dota2.net" {
		return ""
	}
	return u
}
func decoded(b []byte) (any, error) {
	var x any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	e := d.Decode(&x)
	return x, e
}
func round(v float64) float64 { return math.Round(v*100) / 100 }
func normGame(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "cs", "csgo", "cs2", "counter-strike 2", "730":
		return "cs2"
	case "dota", "dota2", "570", "dota 2":
		return "dota2"
	}
	return ""
}
func parseWhen(v any) time.Time {
	if n, ok := asNumber(v); ok && n > 1000000000 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(int64(n), 0).UTC()
	}
	s := asText(v)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
func (a *App) getURL(ctx context.Context, rawURL, method, body, header, prefix, key string) (any, error) {
	u, e := url.Parse(rawURL)
	if e != nil || u.Scheme != "https" || u.Hostname() != "api-pulse.tradeon.space" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("Pulse API: HTTPS URL должен вести точно на api-pulse.tradeon.space")
	}
	for k := range u.Query() {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "auth") || strings.Contains(lower, "password") {
			return nil, errors.New("нельзя помещать ключ API в URL")
		}
	}
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)
	if method != "GET" && method != "POST" {
		return nil, errors.New("допускаются только GET или POST")
	}
	if len(body) > 100000 {
		return nil, errors.New("POST body слишком длинный")
	}
	if method == "POST" {
		if !json.Valid([]byte(body)) {
			return nil, errors.New("POST body должен быть корректным JSON")
		}
	} else if strings.TrimSpace(body) != "" {
		return nil, errors.New("у GET запроса не должно быть body")
	}
	var data io.Reader
	if method == "POST" {
		data = strings.NewReader(body)
	}
	req, e := http.NewRequestWithContext(ctx, method, rawURL, data)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PulseArbitrageAgent/0.4")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		if header == "" {
			header = "Api-Key"
		}
		for _, r := range header {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return nil, errors.New("некорректное имя заголовка авторизации")
			}
		}
		req.Header.Set(header, prefix+key)
	}
	response, e := a.client.Do(req)
	if e != nil {
		return nil, errors.New("не удалось подключиться к Pulse: " + e.Error())
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Pulse HTTP %d (%s): проверь путь, метод, авторизацию и лимиты", response.StatusCode, method)
	}
	b, e := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 8<<20 {
		return nil, errors.New("ответ API >8 МБ: требуется пагинация")
	}
	return decoded(b)
}
func (a *App) scan(ctx context.Context, req ScanRequest, cfg Settings) (ScanResult, error) {
	if req.Mode == "pulse" {
		return a.scanPulse(ctx, req, cfg)
	}
	result := ScanResult{Selected: []Candidate{}, All: []Candidate{}, Rejected: []Candidate{}, Time: time.Now().UTC().Format(time.RFC3339)}
	var base, market any
	var e error
	if len(req.Base) > 0 && len(req.Market) > 0 {
		base, e = decoded(req.Base)
		if e != nil {
			return result, fmt.Errorf("BaseInfos JSON: %w", e)
		}
		market, e = decoded(req.Market)
		if e != nil {
			return result, fmt.Errorf("MarketInfo JSON: %w", e)
		}
	} else if req.API.BaseURL != "" && req.API.MarketURL != "" {
		base, e = a.getURL(ctx, req.API.BaseURL, req.API.BaseMethod, req.API.BaseBody, req.API.Header, req.API.Prefix, req.API.Key)
		if e != nil {
			return result, e
		}
		market, e = a.getURL(ctx, req.API.MarketURL, req.API.MarketMethod, req.API.MarketBody, req.API.Header, req.API.Prefix, req.API.Key)
		if e != nil {
			return result, e
		}
	} else {
		return result, errors.New("выбери два JSON-файла либо укажи два настоящих URL API")
	}
	bases, e := rows(base, req.Mapping.BasePath)
	if e != nil {
		return result, fmt.Errorf("BaseInfos: %w", e)
	}
	markets, e := rows(market, req.Mapping.MarketPath)
	if e != nil {
		return result, fmt.Errorf("MarketInfo: %w", e)
	}
	result.BaseCount = len(bases)
	result.MarketCount = len(markets)
	if len(bases) > 0 {
		result.BaseShape = shape(bases[0], 0)
	}
	if len(markets) > 0 {
		result.MarketShape = shape(markets[0], 0)
	}
	m := req.Mapping
	for _, v := range []string{m.BaseID, m.MarketID, m.Name, m.Buff, m.Sale, m.Sales30} {
		if v == "" {
			return result, errors.New("заполни соответствия полей: ID, название, BUFF, Market, продажи 30д")
		}
	}
	if m.Game == "" && m.DefaultGame == "" {
		return result, errors.New("укажи поле игры или игру по умолчанию")
	}
	index := make(map[string]map[string]any, len(markets))
	for _, raw := range markets {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := asText(dotted(row, m.MarketID))
		if id == "" {
			continue
		}
		if _, exists := index[id]; exists {
			return result, fmt.Errorf("дублирующий ID в MarketInfo: %q", id)
		}
		index[id] = row
	}
	now := time.Now().UTC()
	for _, raw := range bases {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := asText(dotted(b, m.BaseID))
		if id == "" {
			continue
		}
		mr, ok := index[id]
		if !ok {
			continue
		}
		result.Matched++
		name := asText(take(b, mr, m.Name))
		game := normGame(asText(take(b, mr, m.Game)))
		if game == "" {
			game = normGame(m.DefaultGame)
		}
		buff, good1 := asNumber(take(b, mr, m.Buff))
		sale, good2 := asNumber(take(b, mr, m.Sale))
		salesFloat, good3 := asNumber(take(b, mr, m.Sales30))
		if !good1 || !good2 || !good3 || buff <= 0 || sale <= 0 || salesFloat < 0 || name == "" || game == "" {
			continue
		}
		cost := buff * cfg.CNYRub
		net := sale * cfg.Factor
		profit := net - cost
		roi := 100 * profit / cost
		stamp := parseWhen(take(b, mr, m.Updated))
		c := Candidate{ID: id, Name: name, Game: game, BUFFCNY: buff, Cost: round(cost), Sale: round(sale), Net: round(net), Profit: round(profit), ROI: round(roi), Sales30: int(salesFloat), StressProfit: round(sale*.95*cfg.Factor - cost), BuffURL: safeLink(take(b, mr, m.BuffURL)), MarketURL: safeLink(take(b, mr, m.MarketURL)), Risk: "Без проверки истории Market"}
		if !stamp.IsZero() {
			c.QuoteTime = stamp.Format(time.RFC3339)
		}
		reasons := []string{}
		if profit <= 0 || round(profit) <= 0 {
			reasons = append(reasons, "прибыль неположительная")
		}
		if cost < cfg.MinPurchase {
			reasons = append(reasons, "себестоимость < минимальной")
		}
		if roi < cfg.MinROI {
			reasons = append(reasons, "ROI < минимального")
		}
		if cfg.MinSales7 > 0 && c.Sales7 < cfg.MinSales7 {
			reasons = append(reasons, "нет достаточных продаж за 7 дней")
		}
		reasons = append(reasons, additionalFilters(c, cfg)...)
		if int(salesFloat) < cfg.MinSales30 {
			reasons = append(reasons, "продаж за 30д недостаточно")
		}
		if c.BuffURL == "" {
			reasons = append(reasons, "нет корректной ссылки BUFF")
		}
		if !stamp.IsZero() {
			if stamp.After(now.Add(3*time.Minute)) || now.Sub(stamp) > 120*time.Minute {
				reasons = append(reasons, "котировка BUFF устарела")
			}
		} else {
			reasons = append(reasons, "время котировки отсутствует — предварительная оценка")
		}
		if len(reasons) > 0 {
			c.Reason = strings.Join(reasons, "; ")
			result.Rejected = append(result.Rejected, c)
			continue
		}
		result.Preliminary++
		result.Selected = append(result.Selected, c)
	}
	sort.Slice(result.Selected, func(i, j int) bool { return result.Selected[i].ROI > result.Selected[j].ROI })
	if req.VerifyMarket && len(result.Selected) > 0 {
		result.Selected, result.Rejected = a.verifyMarket(ctx, result.Selected, result.Rejected, cfg)
		result.Verified = len(result.Selected)
		result.Notice = "Сигналы с проверкой публичной истории Market: оценки не гарантируют продажу или будущую цену."
	} else {
		result.Notice = "Предварительные сигналы: история продаж Market не проверена. Сведения из BaseInfos/MarketInfo требуют подтверждения; API-поля задаёт оператор."
	}
	sort.Slice(result.Selected, func(i, j int) bool { return result.Selected[i].ROI > result.Selected[j].ROI })
	result.SeenCount = result.Matched
	result.finalizeVisible(cfg)
	return result, nil
}

// RecordCandidate tracks how many records survive each financial stage.
// These counters remain available even when the visible list contains no items.
func (r *ScanResult) RecordCandidate(c Candidate, cfg Settings) {
	if r.RejectStats == nil {
		r.RejectStats = map[string]int{}
	}
	if c.Cost <= 0 || c.Sale <= 0 || c.MarketMin <= 0 || c.Sales30 < 0 {
		r.RejectStats["нет цен или статистики"]++
		return
	}
	r.PriceReady++
	if c.Cost < cfg.MinPurchase {
		r.RejectStats["себестоимость ниже порога"]++
		return
	}
	r.CostPassed++
	if c.Sales30 < cfg.MinSales30 || (cfg.MinSales7 > 0 && c.Sales7 < cfg.MinSales7) {
		r.RejectStats["недостаточно продаж"]++
		return
	}
	r.SalesPassed++
	if !isFinite(c.Profit) || c.Profit <= 0 || !isFinite(c.ROI) || c.ROI < cfg.MinROI {
		r.RejectStats["низкая или отрицательная прибыль"]++
		return
	}
	r.ROIPassed++
	if len(additionalFilters(c, cfg)) > 0 {
		r.RejectStats["дополнительные фильтры"]++
	}
}

// finalizeVisible makes "filters" actual filters: rejected, unmatched and zero/negative
// profit positions are excluded from every public list and CSV export.
// Rejection details are kept only transiently for aggregate counters; the UI
// never receives rejected item records.
func (r *ScanResult) finalizeVisible(cfg Settings) {
	visible := make([]Candidate, 0, len(r.Selected))
	for _, c := range r.Selected {
		if c.Cost <= 0 || c.Sale <= 0 || !isFinite(c.Profit) || c.Profit <= 0 ||
			!isFinite(c.ROI) || c.ROI <= 0 || c.ROI < cfg.MinROI ||
			c.Cost < cfg.MinPurchase || c.Sales30 < cfg.MinSales30 ||
			(cfg.MinSales7 > 0 && c.Sales7 < cfg.MinSales7) ||
			len(additionalFilters(c, cfg)) != 0 {
			continue
		}
		if c.Status == "" {
			c.Status = "Подходит"
		}
		visible = append(visible, c)
	}
	r.Selected = visible
	r.All = append([]Candidate{}, visible...)
	if r.SeenCount < len(visible) {
		r.SeenCount = len(visible)
	}
	r.Excluded = r.SeenCount - len(visible)
	r.Rejected = []Candidate{}
}

func (a *App) fetchMarket(ctx context.Context, fullURL string, maxSize int64) (any, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("User-Agent", "PulseArbitrageAgent/0.4")
	resp, e := a.client.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > maxSize {
		return nil, errors.New("Market API ответ слишком большой")
	}
	return decoded(b)
}
func obj(x any) map[string]any { m, _ := x.(map[string]any); return m }
func from(v any, k string) any { return obj(v)[k] }
func priceRub(v any) float64   { x, _ := asNumber(from(v, "RUB")); return x }
func med(v []float64) float64 {
	sort.Float64s(v)
	if len(v) == 0 {
		return 0
	}
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return .5 * (v[len(v)/2-1] + v[len(v)/2])
}
func (a *App) verifyMarket(ctx context.Context, selected, rejected []Candidate, cfg Settings) ([]Candidate, []Candidate) {
	verified := []Candidate{}
	max := 40
	if len(selected) < max {
		max = len(selected)
	}
	for _, game := range []string{"cs2", "dota2"} {
		root := "https://market.csgo.com"
		if game == "dota2" {
			root = "https://market.dota2.net"
		}
		wanted := []Candidate{}
		for _, c := range selected {
			if c.Game == game {
				wanted = append(wanted, c)
			}
		}
		if len(wanted) == 0 {
			continue
		}
		listingsRaw, e1 := a.fetchMarket(ctx, root+"/api/v2/prices/class_instance/RUB.json", 30<<20)
		idsRaw, e2 := a.fetchMarket(ctx, root+"/api/v2/full-history/all.json", 30<<20)
		if e1 != nil || e2 != nil {
			for _, c := range wanted {
				c.Reason = "публичная история Market недоступна"
				rejected = append(rejected, c)
			}
			continue
		}
		listings := map[string]float64{}
		for _, v := range obj(from(listingsRaw, "items")) {
			row := obj(v)
			n := asText(row["market_hash_name"])
			p, ok := asNumber(row["price"])
			if n != "" && ok && p > 0 && (listings[n] == 0 || listings[n] > p) {
				listings[n] = p
			}
		}
		ids := obj(from(idsRaw, "history"))
		for _, c := range wanted {
			if len(verified) >= max {
				break
			}
			ask := listings[c.Name]
			id := asText(ids[c.Name])
			if ask <= 0 || id == "" {
				c.Reason = "нет цены или ID истории Market"
				rejected = append(rejected, c)
				continue
			}
			idNum, e := strconv.Atoi(id)
			if e != nil {
				c.Reason = "некорректный ID истории"
				rejected = append(rejected, c)
				continue
			}
			history, e := a.fetchMarket(ctx, fmt.Sprintf("%s/api/v2/full-history/%d.json", root, idNum), 10<<20)
			if e != nil {
				c.Reason = "ошибка чтения истории Market"
				rejected = append(rejected, c)
				continue
			}
			stats := obj(from(history, "data"))
			updated := parseWhen(from(history, "time"))
			now := time.Now().UTC()
			if updated.IsZero() || now.Sub(updated) > 4*time.Hour {
				c.Reason = "данные Market устарели"
				rejected = append(rejected, c)
				continue
			}
			avg7 := priceRub(from(stats, "average7d"))
			avg30 := priceRub(from(stats, "average30d"))
			sales30 := priceRub(from(stats, "sales30d"))
			if avg7 <= 0 || avg30 <= 0 || sales30 < float64(cfg.MinSales30) {
				c.Reason = "нет средних цен или недостаточно продаж Market"
				rejected = append(rejected, c)
				continue
			}
			days := map[string][]float64{}
			days7 := map[string]bool{}
			lastSale := time.Time{}
			for _, rec := range anySlice(from(stats, "history")) {
				r := anySlice(rec)
				if len(r) < 2 {
					continue
				}
				dt := parseWhen(r[0])
				p, ok := asNumber(r[1])
				if !ok || p <= 0 || dt.Before(now.Add(-30*24*time.Hour)) || dt.After(now) {
					continue
				}
				date := dt.Format("2006-01-02")
				days[date] = append(days[date], p)
				if dt.After(now.Add(-7 * 24 * time.Hour)) {
					days7[date] = true
				}
				if dt.After(lastSale) {
					lastSale = dt
				}
			}
			if len(days) < 8 || len(days7) < 3 || lastSale.Before(now.Add(-48*time.Hour)) {
				c.Reason = "недостаточно регулярных свежих продаж"
				rejected = append(rejected, c)
				continue
			}
			daily := []float64{}
			for _, arr := range days {
				daily = append(daily, med(arr))
			}
			mean := 0.0
			for _, v := range daily {
				mean += v
			}
			mean /= float64(len(daily))
			sq := 0.0
			for _, v := range daily {
				sq += (v - mean) * (v - mean)
			}
			vol := 100 * math.Sqrt(sq/float64(len(daily))) / mean
			if vol > 14 || avg7/avg30 > 1.15 || avg7/avg30 < .88 {
				c.Reason = "резкий тренд или высокая волатильность"
				rejected = append(rejected, c)
				continue
			}
			expected := math.Min(math.Min(avg7, avg30), ask*.99)
			net := expected * cfg.Factor
			profit := net - c.Cost
			roi := 100 * profit / c.Cost
			stress := expected*.95*cfg.Factor - c.Cost
			if roi < cfg.MinROI || stress <= 0 {
				c.Reason = "после проверки Market прибыль недостаточна"
				rejected = append(rejected, c)
				continue
			}
			c.Sale = round(expected)
			c.ExpectedPrice = round(expected)
			c.Net = round(net)
			c.Profit = round(profit)
			c.ROI = round(roi)
			c.StressProfit = round(stress)
			c.Volatility = round(vol)
			c.Sales30 = int(sales30)
			c.Risk = "Проверена история 7/30 дней Market"
			verified = append(verified, c)
		}
	}
	return verified, rejected
}
func anySlice(v any) []any { x, _ := v.([]any); return x }

func (a *App) notifyTelegram(ctx context.Context, opts TelegramOptions, selected []Candidate) error {
	if opts.BotToken == "" || opts.ChatID == "" {
		return errors.New("telegram: missing credentials")
	}
	for _, ch := range opts.BotToken {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == ':' || ch == '-' || ch == '_') {
			return errors.New("telegram: invalid token")
		}
	}
	sent := 0
	for _, c := range selected {
		if sent >= 5 {
			break
		}
		key := c.Game + "|" + c.ID + "|" + c.BuffURL
		a.mu.Lock()
		prev, exists := a.lastNotified[key]
		a.mu.Unlock()
		if exists && time.Since(prev.At) < 12*time.Hour && c.ROI < prev.ROI+3 {
			continue
		}
		message := fmt.Sprintf("Pulse Agent — сигнал (оценка)\n%s [%s]\nBUFF: %.2f ¥; себестоимость %.2f ₽\nMarket: %.2f ₽; чистая прибыль %.2f ₽ (%.2f%%)\nПродаж за 30 дней: %d\nРиск: %s\n%s\n%s", c.Name, c.Game, c.BUFFCNY, c.Cost, c.Sale, c.Profit, c.ROI, c.Sales30, c.Risk, c.BuffURL, c.MarketURL)
		values := url.Values{"chat_id": {opts.ChatID}, "text": {message}}
		request, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+opts.BotToken+"/sendMessage", strings.NewReader(values.Encode()))
		if e != nil {
			return e
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, e := a.client.Do(request)
		if e != nil {
			return errors.New("telegram: connection failed")
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("telegram: HTTP %d", response.StatusCode)
		}
		a.mu.Lock()
		a.lastNotified[key] = alertStamp{At: time.Now(), ROI: c.ROI}
		a.mu.Unlock()
		sent++
	}
	return nil
}
func (a *App) storeResult(result ScanResult) {
	a.mu.Lock()
	a.latest = result
	a.status.Selected = len(result.Selected)
	a.status.LastError = ""
	a.status.LastRun = time.Now().Format(time.RFC3339)
	a.lastRun = time.Now()
	a.running = false
	a.status.Running = false
	a.mu.Unlock()
}
func (a *App) runAsync(req ScanRequest) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	a.running = true
	a.status.Running = true
	cfg := a.cfg
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, e := a.scan(ctx, req, cfg)
		if e != nil {
			a.mu.Lock()
			a.running = false
			a.status.Running = false
			a.status.LastError = e.Error()
			a.status.LastRun = time.Now().Format(time.RFC3339)
			a.lastRun = time.Now()
			a.mu.Unlock()
			return
		}
		if req.Telegram.Enabled && (req.Mode == "pulse" || (req.API.BaseURL != "" && req.API.MarketURL != "")) && len(req.Base) == 0 && len(req.Market) == 0 && len(res.Selected) > 0 {
			if err := a.notifyTelegram(ctx, req.Telegram, res.Selected); err != nil {
				res.Notice += " Telegram: не удалось отправить уведомления (проверь токен, chat_id и сеть)."
			}
		}
		a.storeResult(res)
	}()
}
func (a *App) loop() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for range tick.C {
		a.mu.Lock()
		active := a.status.Active
		should := active && !a.running && (a.lastRun.IsZero() || time.Since(a.lastRun) >= time.Duration(a.cfg.PollMinutes)*time.Minute)
		var copyReq ScanRequest
		if a.activeRequest != nil {
			copyReq = *a.activeRequest
		}
		a.mu.Unlock()
		if should {
			a.runAsync(copyReq)
		}
	}
}
func (a *App) page(w http.ResponseWriter, r *http.Request) {
	b, _ := assets.ReadFile("ui.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func requestJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if e := json.NewDecoder(r.Body).Decode(dest); e != nil {
		jsonResponse(w, 400, map[string]string{"error": "Неверный JSON или превышен лимит размера"})
		return false
	}
	return true
}
func localPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return false
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		u, e := url.Parse(origin)
		if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
			http.Error(w, "Origin blocked", 403)
			return false
		}
	}
	return true
}
func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			a.mu.Lock()
			s := a.cfg
			a.mu.Unlock()
			jsonResponse(w, 200, s)
			return
		}
		if !localPost(w, r) {
			return
		}
		var s Settings
		if !requestJSON(w, r, &s) {
			return
		}
		if e := saveConfig(s); e != nil {
			jsonResponse(w, 400, map[string]string{"error": e.Error()})
			return
		}
		a.mu.Lock()
		a.cfg = s
		a.mu.Unlock()
		jsonResponse(w, 200, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("/api/all-settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			a.mu.Lock()
			settings, prefs, key, bot := a.cfg, a.prefs, a.apiKey, a.tgToken
			a.mu.Unlock()
			if cloudMode() {
				jsonResponse(w, 200, map[string]any{"settings": settings, "prefs": prefs, "api_key_configured": len(key) > 0, "telegram_token_configured": len(bot) > 0, "secure_store_available": false, "cloud": true})
			} else {
				jsonResponse(w, 200, map[string]any{"settings": settings, "prefs": prefs, "api_key": key, "telegram_token": bot, "secure_store_available": runtime.GOOS == "windows"})
			}
			return
		}
		if !localPost(w, r) {
			return
		}
		var input struct {
			Settings      Settings  `json:"settings"`
			Prefs         UserPrefs `json:"prefs"`
			APIKey        string    `json:"api_key"`
			TelegramToken string    `json:"telegram_token"`
		}
		if !requestJSON(w, r, &input) {
			return
		}
		if err := input.Settings.valid(); err != nil {
			jsonResponse(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if err := input.Prefs.valid(); err != nil {
			jsonResponse(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if len(input.APIKey) > 4096 || len(input.TelegramToken) > 4096 {
			jsonResponse(w, 400, map[string]string{"error": "слишком длинный ключ"})
			return
		}
		if !cloudMode() {
			if err := saveSecret("pulse_api", input.APIKey, input.Prefs.RememberAPI); err != nil {
				jsonResponse(w, 500, map[string]string{"error": "Не удалось сохранить API-ключ: " + err.Error()})
				return
			}
			if err := saveSecret("telegram_bot", input.TelegramToken, input.Prefs.RememberTelegram); err != nil {
				jsonResponse(w, 500, map[string]string{"error": "Не удалось сохранить Telegram-ключ: " + err.Error()})
				return
			}
		}
		if err := saveConfig(input.Settings); err != nil {
			jsonResponse(w, 500, map[string]string{"error": "Не удалось сохранить фильтры: " + err.Error()})
			return
		}
		if err := savePrefs(input.Prefs); err != nil {
			jsonResponse(w, 500, map[string]string{"error": "Не удалось сохранить дополнительные настройки: " + err.Error()})
			return
		}
		a.mu.Lock()
		a.cfg = input.Settings
		a.prefs = input.Prefs
		if !cloudMode() {
			a.apiKey = input.APIKey
			a.tgToken = input.TelegramToken
		} else {
			// Cloud keys are configured exclusively in the host's private environment.
			a.apiKey = os.Getenv("PULSE_API_KEY")
			a.tgToken = os.Getenv("TELEGRAM_BOT_TOKEN")
		}
		a.mu.Unlock()
		jsonResponse(w, 200, map[string]bool{"saved": true})
	})
	mux.HandleFunc("/api/pulse/extra", a.extraHandler)
	mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if !localPost(w, r) {
			return
		}
		var req ScanRequest
		if !requestJSON(w, r, &req) {
			return
		}
		if cloudMode() && req.Mode != "pulse" {
			jsonResponse(w, 400, map[string]string{"error": "В облачном режиме доступен только официальный Pulse API"})
			return
		}
		if cloudMode() {
			// Cloud requests never accept secrets supplied by a browser.
			req.API.Key = ""
			req.Telegram.BotToken = ""
		}
		a.mu.Lock()
		if req.Mode == "pulse" && strings.TrimSpace(req.API.Key) == "" {
			req.API.Key = a.apiKey
		}
		if req.Telegram.Enabled && req.Telegram.BotToken == "" {
			req.Telegram.BotToken = a.tgToken
		}
		if a.running {
			a.mu.Unlock()
			jsonResponse(w, 409, map[string]string{"error": "уже выполняется проверка"})
			return
		}
		a.activeRequest = &req
		a.lastRun = time.Time{}
		a.mu.Unlock()
		a.runAsync(req)
		jsonResponse(w, 202, map[string]string{"ok": "started"})
	})
	mux.HandleFunc("/api/monitor", func(w http.ResponseWriter, r *http.Request) {
		if !localPost(w, r) {
			return
		}
		var req struct {
			Active bool `json:"active"`
		}
		if !requestJSON(w, r, &req) {
			return
		}
		a.mu.Lock()
		if req.Active && (a.activeRequest == nil || (a.activeRequest.Mode != "pulse" && (a.activeRequest.API.BaseURL == "" || a.activeRequest.API.MarketURL == "")) || len(a.activeRequest.Base) > 0 || len(a.activeRequest.Market) > 0) {
			a.mu.Unlock()
			jsonResponse(w, 400, map[string]string{"error": "автомониторинг доступен только после проверки двух настоящих API URL; демо и локальные JSON не обновляются автоматически"})
			return
		}
		a.status.Active = req.Active
		a.mu.Unlock()
		jsonResponse(w, 200, map[string]bool{"active": req.Active})
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		status := a.status
		if status.Active && !a.lastRun.IsZero() {
			status.NextRun = a.lastRun.Add(time.Duration(a.cfg.PollMinutes) * time.Minute).Format(time.RFC3339)
		}
		a.mu.Unlock()
		jsonResponse(w, 200, status)
	})
	mux.HandleFunc("/api/sales-tracker", a.marketSalesHandler)
	mux.HandleFunc("/api/market-diagnostics", a.marketDiagnosticHandler)
	mux.HandleFunc("/api/result", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		v := a.latest
		a.mu.Unlock()
		jsonResponse(w, 200, v)
	})
	mux.HandleFunc("/api/csv", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		v := a.latest
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		all := r.URL.Query().Get("view") == "all"
		filename := "signals.csv"
		if all {
			filename = "filtered_items.csv"
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+filename)
		_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
		c := csv.NewWriter(w)
		_ = c.Write([]string{"item_id", "name", "game", "buff_cny", "cost_rub", "sale_rub", "net_rub", "profit_rub", "roi_pct", "sales30", "sales7", "market_min_rub", "average30_rub", "price_method", "item_type", "quality", "risk", "status", "reason", "buff_url", "market_url"})
		items := v.Selected
		for _, x := range items {
			if x.Profit <= 0 || x.ROI <= 0 {
				continue
			}
			_ = c.Write([]string{safeCSV(x.ID), safeCSV(x.Name), x.Game, fmt.Sprint(x.BUFFCNY), fmt.Sprint(x.Cost), fmt.Sprint(x.Sale), fmt.Sprint(x.Net), fmt.Sprint(x.Profit), fmt.Sprint(x.ROI), fmt.Sprint(x.Sales30), fmt.Sprint(x.Sales7), fmt.Sprint(x.MarketMin), fmt.Sprint(x.Average30), safeCSV(x.PriceMethod), safeCSV(x.ItemType), safeCSV(x.Quality), x.Risk, x.Status, safeCSV(x.Reason), x.BuffURL, x.MarketURL})
		}
		c.Flush()
	})
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if !localPost(w, r) {
			return
		}
		jsonResponse(w, 200, map[string]bool{"stopping": true})
		go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
	})
}
func safeCSV(s string) string {
	t := strings.TrimLeft(s, " \r\t\n")
	if strings.HasPrefix(t, "=") || strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") || strings.HasPrefix(t, "@") {
		return "'" + s
	}
	return s
}
