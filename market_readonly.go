package main

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
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


// An explicit, read-only, authenticated diagnostic for separating a 401 from
// Pulse Agent's Basic Auth and a 401 returned by the external Market API.
// It is never called automatically, reads only two status responses on click,
// and never exposes the API key or any balance / inventory values.
type marketProbeStatus struct {
    Endpoint string `json:"endpoint"`
    HTTPStatus int `json:"http_status,omitempty"`
    APISuccess bool `json:"api_success"`
    Error string `json:"error,omitempty"`
}

func diagnosticMarketProbe(ctx context.Context,client *http.Client,host,path,key string) marketProbeStatus {
    p:=marketProbeStatus{Endpoint:path}
    if e:=marketRequests.acquire(ctx);e!=nil {
        p.Error="Проверка отменена при ожидании лимита запросов"
        return p
    }
    req,e:=http.NewRequestWithContext(ctx,http.MethodGet,"https://"+host+path,nil)
    if e!=nil{p.Error="Ошибка создания запроса";return p}
    req.Header.Set("Accept","application/json")
    req.Header.Set("X-API-KEY",key)
    resp,e:=client.Do(req)
    if e!=nil {p.Error="Не удалось подключиться к Market API";return p}
    defer resp.Body.Close()
    p.HTTPStatus=resp.StatusCode
    if resp.StatusCode!=http.StatusOK {
        p.Error=marketStatusError(resp.StatusCode).Error()
        return p
    }
    // Only inspect the success flag, never return the full private response.
    var decoded struct {Success *bool `json:"success"`}
    decoder:=json.NewDecoder(io.LimitReader(resp.Body,16<<20))
    if e:=decoder.Decode(&decoded);e!=nil{
        p.Error="HTTP 200, но сервер вернул некорректный JSON"
    }else if decoded.Success==nil{
        p.Error="HTTP 200 без поля success: требуется сверить формат ответа"
    }else if !*decoded.Success{
        p.Error="HTTP 200, но Market вернул success=false"
    }else{
        p.APISuccess=true
    }
    return p
}
func (a *App) marketDiagnosticHandler(w http.ResponseWriter,r *http.Request) {
    if r.Method!=http.MethodGet {http.Error(w,"GET only",http.StatusMethodNotAllowed);return}
    platform:=r.URL.Query().Get("platform")
    host,varName:="",""
    switch platform {
    case "dota2":
        host="market.dota2.net";varName="MARKET_DOTA2_API_KEY"
    case "csgo":
        host="market.csgo.com";varName="MARKET_CSGO_API_KEY"
    default:
        jsonResponse(w,400,map[string]string{"error":"Укажи platform=dota2 или platform=csgo"});return
    }
    key:=strings.TrimSpace(os.Getenv(varName))
    data:=map[string]any{
        "origin":"Pulse Agent server",
        "platform":platform,
        "market_host":host,
        "variable":varName,
        "key_configured":key!="",
        "auth_method":"X-API-KEY",
        "rate_limit":"4 requests per second shared across CS2 and Dota2",
    }
    if key=="" {
        data["problem"]="В Render отсутствует ключ этой площадки. Тестовые запросы не выполнялись."
        jsonResponse(w,200,data);return
    }
    fingerprint:=sha256.Sum256([]byte(key))
    // A short non-reversible marker lets the user verify whether a new key
    // actually reached Render, without exposing the key itself.
    data["key_revision"]=hex.EncodeToString(fingerprint[:])[:8]
    client:=&http.Client{Timeout:18*time.Second,CheckRedirect:func(*http.Request,[]*http.Request)error{return http.ErrUseLastResponse}}
    // Both endpoints are GET and cannot buy, sell, or change account state.
    aResult:=diagnosticMarketProbe(r.Context(),client,host,"/api/v2/get-money",key)
    bResult:=diagnosticMarketProbe(r.Context(),client,host,"/api/v2/items",key)
    data["account_check"]=aResult
    data["listings_check"]=bResult
    if bResult.APISuccess {
        marketAuthPermit(host,key)
    }else if bResult.HTTPStatus==http.StatusUnauthorized {
        marketAuthReject(host,key)
    }
    if aResult.HTTPStatus==401 && bResult.HTTPStatus==401 {
        data["problem"]="Market вернул 401 на обоих запросах. Неверный/отозванный ключ, не та площадка или ограничение на стороне Market."
    }else if aResult.APISuccess && bResult.HTTPStatus==401 {
        data["problem"]="Ключ принят методом get-money, но метод items вернул 401: проблема с разрешением на метод или его доступностью."
    }else if aResult.APISuccess && bResult.APISuccess {
        data["problem"]="Авторизация Market работает. Если в главной вкладке всё ещё 401, проверь авторизацию Pulse Agent или кеш."
    }else if aResult.HTTPStatus==0 || bResult.HTTPStatus==0 {
        data["problem"]="Один или оба запроса не дошли до API Market: проверь сетевую доступность."
    }else{
        data["problem"]="Сравни статусы двух запросов: данные поступили непосредственно от Market."
    }
    jsonResponse(w,200,data)
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
    // Market public price feeds may use an object keyed by class/instance ID
    // or item name, as well as an array. Both formats are read-only.
    var d struct {
        Success bool `json:"success"`
        Time any `json:"time"`
        Items json.RawMessage `json:"items"`
    }
    dec:=json.NewDecoder(strings.NewReader(string(raw)));dec.UseNumber()
    if e:=dec.Decode(&d);e!=nil{return nil,"",errors.New("неверный формат рыночных цен")}
    if !d.Success{return nil,"",errors.New("Market не предоставил актуальный список цен")}
    prices:=make(map[string]float64)
    take:=func(fallback string,rawValue any){
        name:=fallback
        priceValue:=rawValue
        if obj,ok:=rawValue.(map[string]any);ok {
            if n:=strings.TrimSpace(marketValue(obj["market_hash_name"]));n!="" {name=n}
            priceValue=obj["price"]
        }
        key:=strings.ToLower(strings.TrimSpace(name))
        if _,ok:=names[key];!ok{return}
        price:=marketFloat(priceValue)
        if price>0 && (prices[key]==0 || price<prices[key]) {prices[key]=price}
    }
    var items any
    id:=json.NewDecoder(strings.NewReader(string(d.Items)));id.UseNumber()
    if e:=id.Decode(&items);e!=nil{return nil,"",errors.New("неверный список цен")}
    switch x:=items.(type) {
    case []any:
        for _,entry:=range x {take("",entry)}
    case map[string]any:
        for key,entry:=range x {take(key,entry)}
    default:
        return nil,"",errors.New("неподдерживаемый формат списка цен")
    }
    stamp:=""
    ts,e:=strconv.ParseInt(marketValue(d.Time),10,64)
    if e==nil && ts>0 {stamp=time.Unix(ts,0).UTC().Format(time.RFC3339)}
    return prices,stamp,nil
}
// Market API explicitly supports X-API-KEY. Authentication failures are not
// silently retried or exposed with secrets in logs or URLs.
// Market does not document automatic renewal of a permanent API key.
// Avoid repeated 401 requests until its server-side value changes, or
// a deliberate read-only diagnostic confirms the same key is valid again.
// Only SHA-256 digests are held in process memory, never secret values.
var errMarketUnauthorized = errors.New("HTTP 401: Market отклонил API-ключ")
var marketAuthGate = struct {
    sync.Mutex
    rejected map[string][32]byte
}{rejected:make(map[string][32]byte)}

func marketAuthBlocked(host,key string) bool {
    marketAuthGate.Lock()
    defer marketAuthGate.Unlock()
    digest,seen:=marketAuthGate.rejected[host]
    return seen && digest==sha256.Sum256([]byte(key))
}
func marketAuthReject(host,key string) {
    marketAuthGate.Lock()
    defer marketAuthGate.Unlock()
    marketAuthGate.rejected[host]=sha256.Sum256([]byte(key))
}
func marketAuthPermit(host,key string) {
    marketAuthGate.Lock()
    defer marketAuthGate.Unlock()
    digest,seen:=marketAuthGate.rejected[host]
    if seen && digest==sha256.Sum256([]byte(key)) {
        delete(marketAuthGate.rejected,host)
    }
}

func marketStatusError(status int) error {
    switch status {
    case http.StatusUnauthorized:
        return fmt.Errorf("%w. Проверь ключ этой площадки и переменную MARKET_DOTA2_API_KEY или MARKET_CSGO_API_KEY в Render", errMarketUnauthorized)
    case http.StatusForbidden:
        return errors.New("HTTP 403: для API-ключа нет доступа к этому методу или аккаунту")
    case http.StatusTooManyRequests:
        return errors.New("HTTP 429: превышен лимит запросов Market API; новые запросы к площадке временно ограничены")
    default:
        return fmt.Errorf("HTTP %d от Market API",status)
    }
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
    if resp.StatusCode!=http.StatusOK {return nil,marketStatusError(resp.StatusCode)}
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
        if marketAuthBlocked(src.host,key) {
            acc.source.Error="Market ранее вернул HTTP 401. Повторные запросы приостановлены. После изменения ключа в Render агент автоматически попробует новый. Для проверки прежнего ключа используй «Проверить авторизацию»."
            accounts=append(accounts,acc)
            continue
        }
        b,e:=marketGet(ctx,client,src.host,"/api/v2/items",key,16<<20)
        if e!=nil {
            if errors.Is(e,errMarketUnauthorized) {
                marketAuthReject(src.host,key)
            }
            acc.source.Error=e.Error()
            accounts=append(accounts,acc)
            continue
        }
        marketAuthPermit(src.host,key)
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
