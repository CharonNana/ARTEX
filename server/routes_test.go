package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemovedUpdateRoutes(t *testing.T) {
	s := &Server{jwtKey: []byte("test-only-signing-key")}
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/update/check"},
		{http.MethodPost, "/api/update/apply"},
		{http.MethodPost, "/api/update/rollback"},
		{http.MethodGet, "/api/update/stream"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status=%d, want 404; body=%s", w.Code, w.Body)
			}
		})
	}
}

func TestHealthKeepsBuildVersion(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || !body.OK || body.Service != "artex" || body.Version != BuildVersion {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
