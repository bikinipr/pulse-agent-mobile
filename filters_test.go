package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestSaleModelsUse30dAndMarketMin(t *testing.T) {
	c := defaults()
	if v := estimateSale(2100, 2000, c); math.Abs(v-2000) > 1e-6 {
		t.Fatalf("safe: %.2f", v)
	}
	if v := estimateSale(1900, 2000, c); math.Abs(v-1881) > 1e-6 {
		t.Fatalf("min ask safe: %.2f", v)
	}
	c.SaleModel = "min"
	if v := estimateSale(2100, 2000, c); math.Abs(v-2079) > 1e-6 {
		t.Fatalf("min model: %.2f", v)
	}
	c.SaleModel = "average"
	if v := estimateSale(2100, 2000, c); math.Abs(v-1980) > 1e-6 {
		t.Fatalf("average model: %.2f", v)
	}
}

func TestExtendedFilters(t *testing.T) {
	s := defaults()
	s.MaxPurchase = 2000
	s.MaxROI = 25
	s.ItemType = "rifle"
	s.Quality = "classified"
	s.Blacklist = "Sticker, Forbidden"
	c := Candidate{Name: "Normal Rifle", Cost: 1900, ROI: 20, ItemType: "Rifle", Quality: "Classified"}
	if len(additionalFilters(c, s)) != 0 {
		t.Fatalf("unexpected rejection: %v", additionalFilters(c, s))
	}
	c.Name = "Forbidden Rifle"
	c.Cost = 2100
	c.ROI = 30
	reasons := strings.Join(additionalFilters(c, s), "|")
	for _, needle := range []string{"чёрном списке", "дороже максимальной", "ROI выше"} {
		if !strings.Contains(reasons, needle) {
			t.Fatalf("missing %s in %s", needle, reasons)
		}
	}
}

func TestSaleEstimateFromBaseAndSevenDaySales(t *testing.T) {
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	buff := map[string]any{"name": "Skin", "type": "Rifle", "quality": "Classified", "sellOffers": map[string]any{"bestOffer": 100}, "marketUrl": "https://buff.163.com/goods/1", "updateTime": stamp}
	tm := map[string]any{"name": "Skin", "sellOffers": map[string]any{"bestOffer": 1900}, "statisticsInfo": map[string]any{"averagePrice30Day": 2000, "sales7Day": 15, "sales30Day": 100}, "marketUrl": "https://market.csgo.com/item/1", "updateTime": stamp}
	cfg := defaults()
	cfg.MinSales7 = 20
	c, ok := candidateFromBaseInfo("CsGo", buff, tm, cfg, now)
	if ok || c.Sales7 != 15 || c.MarketMin != 1900 || c.Average30 != 2000 || c.Sale != 1881 || !strings.Contains(c.Reason, "за 7 дней") {
		t.Fatalf("bad extended quote %+v", c)
	}
	cfg.MinSales7 = 10
	c, ok = candidateFromBaseInfo("CsGo", buff, tm, cfg, now)
	if !ok || c.PriceMethod != "safe" || c.ItemType != "Rifle" || c.Sales7 != 15 {
		t.Fatalf("should pass %+v", c)
	}
}

func TestPresetExclusionsDoNotMatchFinishName(t *testing.T) {
	if excludedByTerm(Candidate{Name: "Example | Case Pattern", ItemType: "Rifle"}, "Case") {
		t.Fatal("case preset excluded a non-case item")
	}
	if !excludedByTerm(Candidate{Name: "Example Case", ItemType: "Container"}, "Case") {
		t.Fatal("case preset missed a container")
	}
	if !excludedByTerm(Candidate{Name: "Sticker | Sample", ItemType: "Sticker"}, "Sticker") {
		t.Fatal("sticker preset missed a sticker")
	}
}
