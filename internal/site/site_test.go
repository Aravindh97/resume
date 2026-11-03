package site

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTest(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{ID: "n1", Region: "test", Public: "127.0.0.1:0", Private: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPublicRoutes(t *testing.T) {
	h := newTest(t).publicHandler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Aravindh Varadharaj") {
		t.Fatalf("index: %d", w.Code)
	}
	if w := get("/api/v1/ask?q=gstin+checksum"); w.Code != 200 || !strings.Contains(w.Body.String(), "sutra/") {
		t.Fatalf("ask: %d %s", w.Code, w.Body.String()[:200])
	}
	if w := get("/api/v1/ask?q=x"); w.Code != 400 {
		t.Fatalf("short query: %d", w.Code)
	}
	if w := get("/api/v1/career?cmd=init+/tmp/x"); w.Code != 400 {
		t.Fatal("career init must not run on the server")
	}
	if w := get("/shots/paisa.jpg"); w.Code != 200 {
		t.Fatalf("React build assets at the root: %d", w.Code)
	}
	if w := get("/api/v1/profile"); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("profile must be JSON so the page can detect server mode: %d", w.Code)
	}
	if w := get("/static/app.wasm"); w.Code != 200 || w.Header().Get("Content-Type") != "application/wasm" {
		t.Fatalf("wasm: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	// Raft RPCs exist only on the private listener.
	for _, p := range []string{"/raft/status", "/raft/isolate", "/raft/append"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s reachable on public listener: %d", p, w.Code)
		}
	}
}

func TestRateLimit(t *testing.T) {
	h := newTest(t).publicHandler()
	limited := 0
	for i := 0; i < 100; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no rate limiting")
	}
}

func TestSpoofedForwardedForIsIgnored(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if ip := clientIP(r); ip == "1.2.3.4" {
		t.Fatal("trusted X-Forwarded-For")
	}
}
