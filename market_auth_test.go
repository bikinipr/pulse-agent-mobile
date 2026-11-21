package main

import (
    "net/http"
    "strings"
    "testing"
)

func TestMarketStatusError401DoesNotExposeSecrets(t *testing.T) {
    msg:=marketStatusError(http.StatusUnauthorized).Error()
    if !strings.Contains(msg,"401") || !strings.Contains(msg,"API-ключ") {
        t.Fatalf("HTTP 401 must explain authentication failure: %q",msg)
    }
    if !strings.Contains(msg,"MARKET_DOTA2_API_KEY") {
        t.Fatalf("HTTP 401 should mention correct Render environment variable")
    }
}

func TestMarketQuoteFormats(t *testing.T) {
    name:="Test Cosmetic"
    wanted:=map[string]struct{}{strings.ToLower(name):{}}
    cases:=[]struct{name string;payload string;want float64}{
        {"array",`{"success":true,"time":1784700000,"items":[{"market_hash_name":"Test Cosmetic","price":"16.20"}]}`,16.20},
        {"object-indexed-id",`{"success":true,"time":1784700000,"items":{"112_0":{"market_hash_name":"Test Cosmetic","price":"17.40"}}}`,17.40},
        {"object-indexed-name",`{"success":true,"time":1784700000,"items":{"Test Cosmetic":{"price":"18.50"}}}`,18.50},
        {"object-price-value",`{"success":true,"time":1784700000,"items":{"Test Cosmetic":"19.60"}}`,19.60},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T) {
            quotes,at,err:=parseMarketQuotes([]byte(tc.payload),wanted)
            if err!=nil {t.Fatal(err)}
            if quotes[strings.ToLower(name)]!=tc.want {t.Fatalf("got %v want %v",quotes,tc.want)}
            if at=="" {t.Fatal("no timestamp")}
        })
    }
}
