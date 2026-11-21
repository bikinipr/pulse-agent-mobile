package main

import (
    "errors"
    "net/http"
    "testing"
)

func TestMarketUnauthorizedIsTyped(t *testing.T) {
    e:=marketStatusError(http.StatusUnauthorized)
    if !errors.Is(e,errMarketUnauthorized) {
        t.Fatalf("401 must be recognizable with errors.Is, got %v",e)
    }
    if errors.Is(marketStatusError(http.StatusForbidden),errMarketUnauthorized) {
        t.Fatal("403 is not an invalid-key response")
    }
}

func TestMarketCredentialRotationAutomaticallyUnblocks(t *testing.T) {
    host:="test.market.example"
    oldKey:="test-secret-old"
    newKey:="test-secret-new"
    marketAuthReject(host,oldKey)
    defer marketAuthPermit(host,oldKey)
    defer marketAuthPermit(host,newKey)
    if !marketAuthBlocked(host,oldKey) {
        t.Fatal("rejected key should be paused")
    }
    if marketAuthBlocked(host,newKey) {
        t.Fatal("a new key must be tried without manual server reset")
    }
    marketAuthPermit(host,oldKey)
    if marketAuthBlocked(host,oldKey) {
        t.Fatal("successful explicit diagnosis must unblock current key")
    }
}

func TestMarketCredentialGateIsPerPlatform(t *testing.T) {
    host:="test-dota.market.example"
    other:="test-csgo.market.example"
    key:="test-secret"
    marketAuthReject(host,key)
    defer marketAuthPermit(host,key)
    if marketAuthBlocked(other,key) {
        t.Fatal("one rejected marketplace must not block the other")
    }
}
