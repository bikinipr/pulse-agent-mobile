package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

// All TradeOn endpoints are fixed and embedded, following the official GitBook documentation.
// Never interpolate user-provided URLs or API keys into these addresses.
const pulseHost = "https://api-pulse.tradeon.space"
const pulseBaseInfoPath = "/public-api/item/base-info"
const pulseMarketInfoPath = "/public-api/item/market-info"
const pulseAdvancedInfoPath = "/public-api/item/advanced-info"
const pulseHistoryPath = "/public-api/item/get-history"
const pulseCompareTablesPath = "/public-api/item/compare-tables"
const pulseCompareBestPath = "/public-api/item/compare-best-offers"
const pulseSupportedMarketsPath = "/public-api/info/supported-markets"
const pulseCurrenciesPath = "/public-api/info/supported-currencies"
const pulseMethodCostsPath = "/public-api/info/method-costs"
const pulseCommissionsPath = "/public-api/info/default-commissions"

// PulseOptions limits the number of requests per cycle to control API-token costs.
type PulseOptions struct {
	Pages             int    `json:"pages"`
	PageSize          int    `json:"page_size"`
	Game              string `json:"game"`
	IncludeMarketInfo bool   `json:"include_market_info"`
}

func (p PulseOptions) normalized() (PulseOptions, error) {
	if p.Pages == 0 {
		p.Pages = 1
	}
	if p.PageSize == 0 {
		p.PageSize = 100
	}
	if p.Game == "" {
		p.Game = "both"
	}
	if p.Pages < 1 || p.Pages > 5 || p.PageSize < 1 || p.PageSize > 500 {
		return p, errors.New("страниц должно быть 1–5, размер страницы 1–500")
	}
	if p.Game != "both" && p.Game != "CsGo" && p.Game != "Dota2" {
		return p, errors.New("игра: CsGo, Dota2 или both")
	}
	return p, nil
}

func (p PulseOptions) games() []string {
	if p.Game == "CsGo" || p.Game == "Dota2" {
		return []string{p.Game}
	}
	return []string{"CsGo", "Dota2"}
}

func pulseBaseURL(market, game, currency string, skip, take int) string {
	v := url.Values{}
	v.Set("Market", market)
	v.Set("Game", game)
	v.Set("Currency", currency)
	v.Set("SkipCount", fmt.Sprint(skip))
	v.Set("TakeCount", fmt.Sprint(take))
	v.Set("Order", "Name")
	return pulseHost + pulseBaseInfoPath + "?" + v.Encode()
}
func pulseMarketURL(game string) string {
	return pulseHost + pulseMarketInfoPath + "?" + url.Values{"Game": {game}}.Encode()
}

func pulseArray(v any) ([]any, error) {
	if a, ok := v.([]any); ok {
		return a, nil
	}
	return rows(v, "")
}

func pulseName(r any) string { return strings.TrimSpace(asText(from(r, "name"))) }

// Key matching tolerates whitespace and case drift between API data sets,
// but intentionally does not remove wear, StatTrak, quality or other attributes.
func pulseItemKey(name string) string {
	name = strings.ReplaceAll(name, "\u00a0", " ")
	name = strings.ReplaceAll(name, "\u200b", "")
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}
func numberAt(r any, p string) (float64, bool) { return asNumber(dotted(r, p)) }
func pulseSafeDate(r any) time.Time            { return parseWhen(from(r, "updateTime")) }
func pulseMarketRecord(rows []any) (map[string]map[string]any, int) {
	result := map[string]map[string]any{}
	duplicates := 0
	for _, raw := range rows {
		m := obj(raw)
		key := pulseItemKey(pulseName(m))
		if key == "" {
			continue
		}
		if existing, found := result[key]; found {
			// Identical records can occur across pages when market data refreshes
			// during pagination. Keep the newer observation instead of deleting both.
			duplicates++
			if pulseSafeDate(m).After(pulseSafeDate(existing)) {
				result[key] = m
			}
			continue
		}
		result[key] = m
	}
	return result, duplicates
}

// Best listed ask is the buy price on BUFF; market sale expectation is capped
// by both the 30-day realized average and the current cheapest ask with a 1% buffer.
func candidateFromBaseInfo(game string, buff, tm map[string]any, cfg Settings, now time.Time) (Candidate, bool) {
	name := pulseName(buff)
	c := Candidate{Name: name, ID: name, Game: normGame(game), BuffURL: buffLink(from(buff, "marketUrl")), MarketURL: tmLink(from(tm, "marketUrl")), Risk: "Предварительно: графики 7/30д не проверены", Status: "Не проверен", ItemType: asText(from(buff, "type")), Quality: asText(from(buff, "quality"))}
	if pulseItemKey(name) == "" || pulseItemKey(pulseName(tm)) != pulseItemKey(name) {
		c.Reason = "предметы двух площадок не сопоставлены"
		return c, false
	}
	buy, okBuy := numberAt(buff, "sellOffers.bestOffer")
	ask, okAsk := numberAt(tm, "sellOffers.bestOffer")
	avg30, okAvg := numberAt(tm, "statisticsInfo.averagePrice30Day")
	sold, okSold := numberAt(tm, "statisticsInfo.sales30Day")
	sold7, okSold7 := numberAt(tm, "statisticsInfo.sales7Day")
	if !okBuy || !okAsk || !okSold || buy <= 0 || ask <= 0 || sold < 0 || sold != math.Trunc(sold) {
		c.Reason = "неполные цены или статистика в BaseInfos"
		c.Status = "Нет данных"
		return c, false
	}
	c.BUFFCNY = round(buy)
	c.Cost = round(buy * cfg.CNYRub)
	c.MarketMin = round(ask)
	c.Sales30 = int(sold)
	if okSold7 && sold7 >= 0 && sold7 == math.Trunc(sold7) {
		c.Sales7 = int(sold7)
	}
	warnings := []string{}
	// Falling back to the currently listed ask is allowed but visibly marked.
	// Never silently label the fallback as a measured 30-day average.
	forEstimate := avg30
	if !okAvg || avg30 <= 0 {
		forEstimate = ask
		warnings = append(warnings, "нет средней цены 30д: оценка по текущему предложению")
	} else {
		c.Average30 = round(avg30)
	}
	c.PriceMethod = cfg.SaleModel
	c.Sale = round(estimateSale(ask, forEstimate, cfg))
	c.ExpectedPrice = c.Sale
	c.Net = round(c.Sale * cfg.Factor)
	c.Profit = round(c.Sale*cfg.Factor - buy*cfg.CNYRub)
	c.ROI = round(100 * (c.Sale*cfg.Factor - buy*cfg.CNYRub) / (buy * cfg.CNYRub))
	c.StressProfit = round(c.Sale*.95*cfg.Factor - buy*cfg.CNYRub)
	stampBuff, stampTm := pulseSafeDate(buff), pulseSafeDate(tm)
	if stampBuff.IsZero() || stampTm.IsZero() {
		warnings = append(warnings, "отсутствует время обновления котировки")
	} else {
		c.QuoteTime = stampBuff.Format(time.RFC3339)
		if stampBuff.After(now.Add(3*time.Minute)) || stampTm.After(now.Add(3*time.Minute)) || now.Sub(stampBuff) > 2*time.Hour || now.Sub(stampTm) > 2*time.Hour {
			warnings = append(warnings, "время котировки требует перепроверки")
		}
	}
	if c.BuffURL == "" || c.MarketURL == "" {
		warnings = append(warnings, "нет подтверждённой прямой ссылки одной из площадок")
	}
	if len(warnings) > 0 {
		c.Risk += "; " + strings.Join(warnings, "; ")
	}
	reasons := []string{}
	if c.Cost < cfg.MinPurchase {
		reasons = append(reasons, "себестоимость ниже порога")
	}
	if c.Profit <= 0 || !isFinite(c.Profit) || c.Net <= c.Cost {
		reasons = append(reasons, "прибыль нулевая или отрицательная")
	}
	if !isFinite(c.ROI) || c.ROI < cfg.MinROI {
		reasons = append(reasons, "ROI ниже порога")
	}
	if c.Sales30 < cfg.MinSales30 {
		reasons = append(reasons, "меньше необходимого числа продаж за 30 дней")
	}
	if cfg.MinSales7 > 0 && (!okSold7 || sold7 < float64(cfg.MinSales7)) {
		reasons = append(reasons, "меньше необходимого числа продаж за 7 дней или нет данных")
	}
	reasons = append(reasons, additionalFilters(c, cfg)...)
	// A 5% price-drop stress test is a warning, not a hidden eligibility rule.
	if c.StressProfit <= 0 {
		c.Risk += "; возможен убыток при снижении цены на 5%"
	}
	if len(reasons) > 0 {
		c.Reason = strings.Join(reasons, "; ")
		c.Status = "Не подходит"
		return c, false
	}
	c.Status = "Подходит предварительно"
	return c, true
}

func (a *App) scanPulse(ctx context.Context, r ScanRequest, cfg Settings) (ScanResult, error) {
	out := ScanResult{Selected: []Candidate{}, All: []Candidate{}, Rejected: []Candidate{}, Time: time.Now().UTC().Format(time.RFC3339)}
	p, e := r.Pulse.normalized()
	if e != nil {
		return out, e
	}
	key := strings.TrimSpace(r.API.Key)
	if key == "" {
		return out, errors.New("укажи API-ключ Pulse в поле интерфейса")
	}
	now := time.Now().UTC()
	totalRequests := 0
	metadataCount := 0
	types := map[string]bool{}
	qualities := map[string]bool{}
	for _, game := range p.games() {
		allBuff := []any{}
		allTm := []any{}
		for page := 0; page < p.Pages; page++ {
			for _, market := range []struct {
				key, cur string
				dest     *[]any
			}{{"Buff", "CNY", &allBuff}, {"Tm", "RUB", &allTm}} {
				path := pulseBaseURL(market.key, game, market.cur, page*p.PageSize, p.PageSize)
				payload, err := a.getURL(ctx, path, "GET", "", "Api-Key", "", key)
				if err != nil {
					return out, fmt.Errorf("BaseInfos %s %s, страница %d: %w", market.key, game, page+1, err)
				}
				totalRequests++
				values, err := pulseArray(payload)
				if err != nil {
					return out, fmt.Errorf("BaseInfos %s %s: неожиданный формат JSON: %w", market.key, game, err)
				}
				*market.dest = append(*market.dest, values...)
				if len(values) == 0 && page == 0 { // no results is valid for an empty market, not an auth error
				}
			}
		}
		out.BaseCount += len(allBuff)
		out.MarketCount += len(allTm)
		if len(allBuff) > 0 && out.BaseShape == nil {
			out.BaseShape = shape(allBuff[0], 0)
		}
		if len(allTm) > 0 && out.MarketShape == nil {
			out.MarketShape = shape(allTm[0], 0)
		}
		tmIndex, dupTm := pulseMarketRecord(allTm)
		if dupTm > 0 {
			out.Notice += fmt.Sprintf(" %s: %d повторных записей Market объединены.", game, dupTm)
		}
		seenBuff := map[string]bool{}
		for _, raw := range allBuff {
			b := obj(raw)
			name := pulseName(b)
			if t := strings.TrimSpace(asText(from(b, "type"))); t != "" {
				types[t] = true
			}
			if q := strings.TrimSpace(asText(from(b, "quality"))); q != "" {
				qualities[q] = true
			}
			if name == "" {
				continue
			}
			// Track every unique BUFF listing for aggregate counts; display only eligible ones.
			keyName := pulseItemKey(name)
			if seenBuff[keyName] {
				continue
			}
			seenBuff[keyName] = true
			out.SeenCount++
			m, matched := tmIndex[keyName]
			if !matched {
				out.Unmatched++
				continue
			}
			out.Matched++
			c, pass := candidateFromBaseInfo(game, b, m, cfg, now)
			out.RecordCandidate(c, cfg)
			if pass {
				out.Selected = append(out.Selected, c)
				out.Preliminary++
			} else {
				out.Rejected = append(out.Rejected, c)
			}
		}
		if p.IncludeMarketInfo {
			metadata, err := a.getURL(ctx, pulseMarketURL(game), "GET", "", "Api-Key", "", key)
			if err != nil {
				out.Notice += fmt.Sprintf(" MarketInfo %s: ошибка (не влияет на цены).", game)
			} else if list, err := pulseArray(metadata); err == nil {
				totalRequests++
				metadataCount += len(list)
			} else {
				out.Notice += fmt.Sprintf(" MarketInfo %s: неожиданный JSON.", game)
			}
		}
	}
	if out.Matched == 0 && out.BaseCount > 0 && out.MarketCount > 0 {
		out.Notice += " Внимание: данные обеих площадок получены, но общих названий в загруженных страницах нет; попробуй расширить диапазон сканирования или сравни названия в ответах API."
	} else if out.Matched > 0 && out.PriceReady == 0 {
		out.Notice += " Внимание: совпадения есть, но ни одно не содержит достаточных числовых цен и статистики. Проверь структуру ответа BaseInfos."
	} else if out.PriceReady > 0 && out.ROIPassed == 0 {
		out.Notice += " Внимание: цены получены, но ни одна позиция не прошла торговые фильтры. Смотри счётчики и причины исключения ниже."
	}
	// Published method costs: BaseInfos and MarketInfo are 2 tokens each.
	out.Notice = fmt.Sprintf("TradeOn API: %d успешных запросов, ориентировочно %d токенов; MarketInfo записей: %d. Просканировано до %d страниц по %d предметов с каждой площадки и игры, не весь рынок. Совпало: %d, без пары: %d; данные для расчёта: %d, себестоимость прошла: %d, ликвидность прошла: %d, ROI прошёл: %d. Предложения без ссылок и с устаревшим временем теперь помечаются предупреждением, а не удаляются. %s", totalRequests, totalRequests*2, metadataCount, p.Pages, p.PageSize, out.Matched, out.Unmatched, out.PriceReady, out.CostPassed, out.SalesPassed, out.ROIPassed, out.Notice)
	sort.Slice(out.Selected, func(i, j int) bool { return out.Selected[i].ROI > out.Selected[j].ROI })
	sort.Slice(out.Rejected, func(i, j int) bool { return out.Rejected[i].Name < out.Rejected[j].Name })
	if r.VerifyMarket && len(out.Selected) > 0 {
		out.Selected, out.Rejected = a.verifyMarket(ctx, out.Selected, out.Rejected, cfg)
		out.Verified = len(out.Selected)
		out.Notice += " Строгая проверка истории Market включена; если данные недоступны, сигналы отбрасываются."
	}
	for k := range types {
		out.ItemTypes = append(out.ItemTypes, k)
	}
	for k := range qualities {
		out.Qualities = append(out.Qualities, k)
	}
	sort.Strings(out.ItemTypes)
	sort.Strings(out.Qualities)
	out.finalizeVisible(cfg)
	return out, nil
}
