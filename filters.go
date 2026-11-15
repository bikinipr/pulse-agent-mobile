package main

import (
	"math"
	"strings"
)

// estimateSale uses documented BaseInfo prices. It is an estimate,
// not an executed trade or a prediction of next week's price.
func estimateSale(minAsk, average30 float64, cfg Settings) float64 {
	if !isFinite(minAsk) || !isFinite(average30) || minAsk <= 0 || average30 <= 0 {
		return 0
	}
	discount := 1 - cfg.DiscountPct/100
	switch cfg.SaleModel {
	case "min":
		return minAsk * discount
	case "average":
		return average30 * discount
	default:
		return math.Min(minAsk*discount, average30)
	}
}

func additionalFilters(c Candidate, cfg Settings) []string {
	out := []string{}
	if cfg.MaxPurchase > 0 && c.Cost > cfg.MaxPurchase {
		out = append(out, "дороже максимальной себестоимости")
	}
	if cfg.MaxROI > 0 && c.ROI > cfg.MaxROI {
		out = append(out, "ROI выше верхнего лимита")
	}
	if cfg.ItemType != "" && !strings.Contains(strings.ToLower(c.ItemType), strings.ToLower(strings.TrimSpace(cfg.ItemType))) {
		out = append(out, "тип предмета не соответствует фильтру")
	}
	if cfg.Quality != "" && !strings.Contains(strings.ToLower(c.Quality), strings.ToLower(strings.TrimSpace(cfg.Quality))) {
		out = append(out, "качество предмета не соответствует фильтру")
	}
	for _, term := range strings.FieldsFunc(cfg.Blacklist, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		term = strings.TrimSpace(term)
		if term != "" && excludedByTerm(c, term) {
			out = append(out, "предмет в чёрном списке")
			break
		}
	}
	return out
}

// The common checkboxes are categories, not arbitrary substring searches.
// An item whose finish contains the word "Case" must not be treated as a case.
func excludedByTerm(c Candidate, term string) bool {
	name := strings.ToLower(strings.TrimSpace(c.Name))
	typ := strings.ToLower(strings.TrimSpace(c.ItemType))
	switch strings.ToLower(strings.TrimSpace(term)) {
	case "case":
		return strings.HasSuffix(name, " case") || strings.HasSuffix(name, " chest") || strings.Contains(typ, "container") || typ == "case"
	case "sticker":
		return strings.HasPrefix(name, "sticker |") || strings.Contains(typ, "sticker")
	case "souvenir":
		return strings.HasPrefix(name, "souvenir ")
	default:
		return strings.Contains(name, strings.ToLower(strings.TrimSpace(term)))
	}
}
