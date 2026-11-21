package main

import (
    "io"
    "net/http"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"
)

func TestMarketLimitRolling60Seconds(t *testing.T) {
    var lim marketRequestLimiter
    base:=time.Unix(100000,0)
    for i:=0;i<5;i++ {
        if ok,_:=lim.reserve(base);!ok {t.Fatalf("request %d must pass",i+1)}
    }
    if ok,_:=lim.reserve(base.Add(59*time.Second));ok {t.Fatal("sixth request must be blocked during the 60s window")}
    if ok,_:=lim.reserve(base.Add(60*time.Second));!ok {t.Fatal("first slot should be available after 60s")}
}

func TestMarketLimitConcurrentRequests(t *testing.T) {
    var lim marketRequestLimiter
    now:=time.Unix(100000,0)
    var accepted atomic.Int32
    var wg sync.WaitGroup
    for i:=0;i<50;i++ {
        wg.Add(1)
        go func(){
            defer wg.Done()
            if ok,_:=lim.reserve(now);ok {accepted.Add(1)}
        }()
    }
    wg.Wait()
    if n:=accepted.Load();n!=5 {t.Fatalf("accepted %d requests, want exactly 5",n)}
}

type fakeMarketTransport struct { sent atomic.Int32 }
func (f *fakeMarketTransport) RoundTrip(r *http.Request) (*http.Response,error) {
    f.sent.Add(1)
    return &http.Response{StatusCode:200,Body:io.NopCloser(strings.NewReader(`{"success":true,"items":[]}`)),Header:make(http.Header),Request:r},nil
}
func TestMarketRequestsBlockedBeforeHTTP(t *testing.T) {
    marketRequests.Lock()
    marketRequests.times=nil
    marketRequests.Unlock()
    fake:=&fakeMarketTransport{}
    client:=&http.Client{Transport:fake}
    for i:=0;i<5;i++ {
        if _,err:=marketGet(t.Context(),client,"market.csgo.com","/api/v2/items","example-test-key",1024);err!=nil {
            t.Fatalf("request %d should be allowed: %v",i+1,err)
        }
    }
    if _,err:=marketGet(t.Context(),client,"market.dota2.net","/api/v2/items","example-test-key",1024);err==nil {
        t.Fatal("sixth request must be skipped across both Market hosts")
    }
    if n:=fake.sent.Load();n!=5 {t.Fatalf("transport sent %d requests, want 5",n)}
}
