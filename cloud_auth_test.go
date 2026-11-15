package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudAuthenticationAndKeyPrivacy(t *testing.T) {
	t.Setenv("CLOUD_MODE", "1")
	t.Setenv("APP_PASSWORD", "example-long-private-admin-password")
	t.Setenv("PULSE_API_KEY", "test-secret-not-for-public")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := newApp()
	m := http.NewServeMux()
	a.routes(m)
	handler := cloudAuth(m)
	request := func(auth bool, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://pulse.example"+path, nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		if auth {
			r.SetBasicAuth("admin", "example-long-private-admin-password")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if r := request(false, "/api/all-settings"); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %d", r.Code)
	}
	r := request(true, "/api/all-settings")
	if r.Code != http.StatusOK {
		t.Fatalf("authenticated request: %d: %s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), "test-secret-not-for-public") {
		t.Fatal("secret leaked in HTTP response")
	}
	var v map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["api_key_configured"] != true {
		t.Fatal("API key status missing")
	}
	if z := request(true, "/api/stop"); z.Code != http.StatusNotFound {
		t.Fatalf("public stop enabled: %d", z.Code)
	}
	if z := request(false, "/health"); z.Code != http.StatusOK {
		t.Fatalf("health failed: %d", z.Code)
	}
}

func TestCloudBlocksUnencryptedAccess(t *testing.T) {
	t.Setenv("CLOUD_MODE", "1")
	t.Setenv("APP_PASSWORD", "example-long-private-admin-password")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://pulse.example/", nil)
	r.SetBasicAuth("admin", "example-long-private-admin-password")
	cloudAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
	if w.Code != http.StatusUpgradeRequired {
		t.Fatalf("unencrypted access accepted: %d", w.Code)
	}
}
