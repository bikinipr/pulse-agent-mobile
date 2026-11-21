package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "math"
    "net/http"
    "os"
    "strconv"
    "strings"
    "sync"
    "time"
)

// Market account APIs are separate from TradeOn Pulse: the former identify
// the user's own active listings, the latter provides public price analytics.
// This module is strictly read-only; no set-price or trading requests.
type marketSaleRow struct {
    ID string `json:"id"`
    Market string `json:"market"`
    Name string `json:"name"`
    Status string `json:"status"`
    Price float64 `json:"price"`
    Currency string `json:"currency"`
    QueuePosition int `json:"queue_position"`
    LowestPrice float64 `json:"lowest_price,omitempty"`
    PriceCheckedAt string `json:"price_checked_at,omitempty"`
    IsBelow bool `json:"is_below"`
    HasQuote bool `json:"has_quote"`
}
type marketSourceStatus struct {
    Market string `json:"market"`
    Connected bool `json:"connected"`
    Count int `json:"count"`
    Error string `json:"error,omitempty"`
}
type marketTrackerData struct {
    GeneratedAt string `json:"generated_at"`
    Listings []marketSaleRow `json:"listings"`
    Sources []marketSourceStatus `json:"sources"`
    Cached bool `json:"cached"`
}
var salesTracker struct {
    sync.Mutex
    Data marketTrackerData
    At time.Time
    Running bool
}
const salesCacheTime = 120 * time.Second

// One shared rolling 1-second rate limit for all Market API calls on this
// server (CS2 + Dota 2, listings + public prices). We deliberately use only
// 4 out of the permitted 5 requests per second to provide safety headroom.
// Calls queue instead of being dropped. Every HTTP attempt consumes a slot.
const marketRequestCap = 4
const marketRequestWindow = time.Second

type marketRequestLimiter struct {
    sync.Mutex
    times []time.Time
}
var marketRequests marketRequestLimiter

func (l *marketRequestLimiter) reserve(now time.Time) (bool, time.Duration) {
    l.Lock()
    defer l.Unlock()
    recent:=l.times[:0]
    for _,t:=range l.times {
        if now.Sub(t)<marketRequestWindow && !now.Before(t) {recent=append(recent,t)}
    }
    l.times=recent
    if len(l.times)>=marketRequestCap {
        wait:=marketRequestWindow-now.Sub(l.times[0])
        if wait<time.Millisecond {wait=time.Millisecond}
        return false,wait
    }
    l.times=append(l.times,now)
    return true,0
}


// acquire waits in a shared queue until the 1-second request budget
// permits another request. The mutex makes reservation safe for concurrent
// clients. Waiting respects cancelled requests and makes no network calls.
func (l *marketRequestLimiter) acquire(ctx context.Context) error {
    for {
        ok,delay:=l.reserve(time.Now())
        if ok {return nil}
        if delay<time.Millisecond {delay=time.Millisecond}
        timer:=time.NewTimer(delay)
        select {
        case <-ctx.Done():
            timer.Stop()
            return ctx.Err()
        case <-timer.C:
        }
    }
}

func (a *App) marketSalesHandler(w http.ResponseWriter,r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w,"GET only",http.StatusMethodNotAllowed);return
    }
    salesTracker.Lock()
    if !salesTracker.At.IsZero() && time.Since(salesTracker.At) < salesCacheTime {
        v:=salesTracker.Data;v.Cached=true
        salesTracker.Unlock();jsonResponse(w,200,v);return
    }
    if salesTracker.Running {
        v:=salesTracker.Data
        salesTracker.Unlock()
        if v.GeneratedAt!="" {
            v.Cached=true;jsonResponse(w,200,v)
        }else{jsonResponse(w,http.StatusServiceUnavailable,map[string]string{"error":"Обновление уже выполняется"})}
        return
    }
    salesTracker.Running=true
    salesTracker.Unlock()
    result:=fetchMarketSales(r.Context())
    salesTracker.Lock()
    salesTracker.Data=result;salesTracker.At=time.Now();salesTracker.Running=false
    salesTracker.Unlock()
    jsonResponse(w,200,result)
}

func marketValue(v any) string {
    switch x:=v.(type) {
    case json.Number: return x.String()
    case string: return x
    case float64: return strconv.FormatFloat(x,'f',-1,64)
    case int: return strconv.Itoa(x)
    default: return ""
    }
}
func marketFloat(v any) float64 {
    x,e:=strconv.ParseFloat(strings.TrimSpace(marketValue(v)),64)
    if e!=nil || math.IsInf(x,0)||math.IsNaN(x)||x<0{return 0}
    return x
}
func marketInt(v any) int {
    n,e:=strconv.Atoi(marketValue(v))
    if e!=nil || n<0{return 0}
    return n
}
func parsePersonalListing(raw []byte,market string) ([]marketSaleRow,error) {
    var d struct {
        Success bool `json:"success"`
        Items []struct {
            ID any `json:"item_id"`
            Name string `json:"market_hash_name"`
            Status any `json:"status"`
            Price any `json:"price"`
            Currency string `json:"currency"`
            Position any `json:"position"`
        } `json:"items"`
    }
    dec:=json.NewDecoder(strings.NewReader(string(raw)));dec.UseNumber()
    if e:=dec.Decode(&d);e!=nil{return nil,errors.New("неверный формат списка объявлений")}
    if !d.Success{return nil,errors.New("Market отклонил запрос: проверь ключ и доступ к API")}
    res:=make([]marketSaleRow,0,len(d.Items))
    for _,v:=range d.Items {
        st:=marketValue(v.Status)
        if st!="1" && st!="2" && st!="7"{continue}
        name:=strings.TrimSpace(v.Name)
        if name==""{continue}
        currency:=strings.ToUpper(strings.TrimSpace(v.Currency))
        if currency=="" {currency="RUB"}
        row:=marketSaleRow{ID:marketValue(v.ID),Market:market,Name:name,Status:st,Price:marketFloat(v.Price),Currency:currency,QueuePosition:marketInt(v.Position)}
        res=append(res,row)
    }
    return res,nil
}
func parseMarketQuotes(raw []byte, names map[string]struct{}) (map[string]float64,string,error) {
    var d struct {
        Success bool `json:"success"`
        Time any `json:"time"`
        Items []struct {
            Name string `json:"market_hash_name"`
            Price any `json:"price"`
        } `json:"items"`
    }
    dec:=json.NewDecoder(strings.NewReader(string(raw)));dec.UseNumber()
    if e:=dec.Decode(&d);e!=nil{return nil,"",errors.New("неверный формат рыночных цен")}
    if !d.Success{return nil,"",errors.New("Market не предоставил актуальный список цен")}
    prices:=make(map[string]float64)
    for _,item:=range d.Items{
        key:=strings.ToLower(strings.TrimSpace(item.Name))
        if _,ok:=names[key];!ok{continue}
        price:=marketFloat(item.Price)
        if price>0 && (prices[key]==0 || price<prices[key]) {prices[key]=price}
    }
    stamp:=""
    ts,e:=strconv.ParseInt(marketValue(d.Time),10,64)
    if e==nil && ts>0 {stamp=time.Unix(ts,0).UTC().Format(time.RFC3339)}
    return prices,stamp,nil
}
func marketGet(ctx context.Context,client *http.Client,host,path,key string,limit int64) ([]byte,error){
    if host!="market.csgo.com" && host!="market.dota2.net" {return nil,errors.New("Неверный хост")}
    req,e:=http.NewRequestWithContext(ctx,http.MethodGet,"https://"+host+path,nil)
    if e!=nil{return nil,e}
    req.Header.Set("Accept","application/json")
    if key!="" {req.Header.Set("X-API-KEY",key)}
    if e:=marketRequests.acquire(ctx);e!=nil {
        return nil,errors.New("Ожидание лимита Market API отменено")
    }
    resp,e:=client.Do(req)
    if e!=nil{return nil,errors.New("Market API недоступен")}
    defer resp.Body.Close()
    if resp.StatusCode!=http.StatusOK {return nil,fmt.Errorf("HTTP %d от Market API",resp.StatusCode)}
    b,e:=io.ReadAll(io.LimitReader(resp.Body,limit+1))
    if e!=nil{return nil,errors.New("Не удалось прочитать ответ Market")}
    if int64(len(b))>limit{return nil,errors.New("Ответ Market слишком большой")}
    return b,nil
}
// Fetch each connected account's own listings BEFORE spending the remaining
// budget on optional public prices. This keeps CS2 and Dota 2 fair when both
// accounts are connected. Price quotes are attempted in RUB, USD, EUR order.
// If the burst budget is full, each subsequent request waits for an available slot.
type marketAccountState struct {
    source marketSourceStatus
    host string
    listings []marketSaleRow
    names map[string]map[string]struct{}
}
func fetchMarketSales(ctx context.Context) marketTrackerData {
    out:=marketTrackerData{GeneratedAt:time.Now().UTC().Format(time.RFC3339),Listings:[]marketSaleRow{},Sources:[]marketSourceStatus{}}
    client:=&http.Client{Timeout:22*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}}
    hosts:=[]struct{market,host,keyVar string}{
        {"CS2","market.csgo.com","MARKET_CSGO_API_KEY"},
        {"Dota 2","market.dota2.net","MARKET_DOTA2_API_KEY"},
    }
    accounts:=make([]marketAccountState,0,len(hosts))
    for _,src:=range hosts {
        key:=strings.TrimSpace(os.Getenv(src.keyVar))
        acc:=marketAccountState{
            source:marketSourceStatus{Market:src.market,Connected:key!=""},
            host:src.host,
            names:make(map[string]map[string]struct{}),
        }
        if key=="" {
            acc.source.Error="Не настроен ключ "+src.keyVar+" в Render"
            accounts=append(accounts,acc)
            continue
        }
        b,e:=marketGet(ctx,client,src.host,"/api/v2/items",key,16<<20)
        if e!=nil {
            acc.source.Error=e.Error()
            accounts=append(accounts,acc)
            continue
        }
        items,e:=parsePersonalListing(b,src.market)
        if e!=nil {
            acc.source.Error=e.Error()
            accounts=append(accounts,acc)
            continue
        }
        acc.source.Count=len(items)
        acc.listings=items
        for _,v:=range items {
            if v.Status!="1" {continue}
            if v.Currency!="RUB"&&v.Currency!="USD"&&v.Currency!="EUR" {continue}
            if acc.names[v.Currency]==nil {acc.names[v.Currency]=make(map[string]struct{})}
            acc.names[v.Currency][strings.ToLower(strings.TrimSpace(v.Name))]=struct{}{}
        }
        accounts=append(accounts,acc)
    }
    // Quotes are optional; no polling loop may bypass the same shared limiter.
    for _,currency:=range []string{"RUB","USD","EUR"} {
        for i:=range accounts {
            acc:=&accounts[i]
            names:=acc.names[currency]
            if len(names)==0 {continue}
            b,e:=marketGet(ctx,client,acc.host,"/api/v2/prices/"+currency+".json","",48<<20)
            if e!=nil {
                acc.source.Error="Не удалось сравнить цены "+currency+": "+e.Error()
                continue
            }
            prices,stamp,err:=parseMarketQuotes(b,names)
            if err!=nil {
                acc.source.Error="Ошибка котировок "+currency+": "+err.Error()
                continue
            }
            for j:=range acc.listings {
                v:=&acc.listings[j]
                if v.Status!="1" || v.Currency!=currency {continue}
                p:=prices[strings.ToLower(strings.TrimSpace(v.Name))]
                if p>0 {
                    v.HasQuote=true
                    v.LowestPrice=p
                    v.PriceCheckedAt=stamp
                    v.IsBelow=p+0.009<v.Price
                }
            }
        }
    }
    for _,acc:=range accounts {
        out.Listings=append(out.Listings,acc.listings...)
        out.Sources=append(out.Sources,acc.source)
    }
    return out
}
