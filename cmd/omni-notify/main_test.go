package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunHealthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	if code := runHealthcheck([]string{"-url", healthy.URL}); code != 0 {
		t.Errorf("healthy endpoint = %d, want 0", code)
	}
	if code := runHealthcheck([]string{"-url", unhealthy.URL}); code != 1 {
		t.Errorf("unhealthy endpoint = %d, want 1", code)
	}
	if code := runHealthcheck([]string{"-url", "http://127.0.0.1:1/nope", "-timeout", "500ms"}); code != 1 {
		t.Errorf("unreachable endpoint = %d, want 1", code)
	}
}
