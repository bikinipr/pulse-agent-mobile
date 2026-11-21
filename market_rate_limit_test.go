package main

import (
    "context"
    "errors"
    "testing"
    "time"
)

func TestMarketRateLimitSlidingSecond(t *testing.T) {
    var limiter marketRequestLimiter
    now:=time.Unix(123456,0)
    for i:=0; i<marketRequestCap; i++ {
        ok,delay:=limiter.reserve(now.Add(time.Duration(i)*10*time.Millisecond))
        if !ok || delay!=0 {
            t.Fatalf("request %d unexpectedly denied: ok=%v delay=%v",i,ok,delay)
        }
    }
    ok,delay:=limiter.reserve(now.Add(50*time.Millisecond))
    if ok || delay<900*time.Millisecond || delay>time.Second {
        t.Fatalf("fifth request should wait about a second: ok=%v delay=%v",ok,delay)
    }
    ok,_=limiter.reserve(now.Add(time.Second+time.Nanosecond))
    if !ok { t.Fatal("oldest request must expire after rolling 1-second window") }
}

func TestMarketLimiterWaitingCanBeCancelled(t *testing.T) {
    var limiter marketRequestLimiter
    now:=time.Now()
    for i:=0;i<marketRequestCap;i++ {
        if ok,_:=limiter.reserve(now); !ok {t.Fatal("failed to fill test budget")}
    }
    ctx,cancel:=context.WithTimeout(context.Background(),30*time.Millisecond)
    defer cancel()
    start:=time.Now()
    err:=limiter.acquire(ctx)
    if !errors.Is(err,context.DeadlineExceeded) {
        t.Fatalf("expected cancellation during queue wait, got %v",err)
    }
    if time.Since(start)>500*time.Millisecond {t.Fatal("cancelled request was blocked too long")}
}

func TestMarketRateLimitIsSharedAcrossAllCalls(t *testing.T) {
    var limiter marketRequestLimiter
    base:=time.Unix(99999,0)
    // Simulates CS2 listings, Dota 2 listings, and two prices requests.
    for _,src:=range []string{"cs2/items","dota2/items","cs2/prices","dota2/prices"} {
        if ok,_:=limiter.reserve(base); !ok {t.Fatalf("shared slot unavailable: %s",src)}
    }
    if ok,_:=limiter.reserve(base.Add(200*time.Millisecond));ok {
        t.Fatal("request for either market must not bypass shared budget")
    }
}
