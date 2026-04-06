package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMakeApiRequest(t *testing.T) {
	expected := map[string]any{
		"temp":   72.5,
		"status": "ok",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(expected)
	}))
	defer srv.Close()

	var result map[string]any
	if err := makeApiRequest(srv.URL, &result); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result["temp"] != 72.5 {
		t.Errorf("temp = %v, want 72.5", result["temp"])
	}
	if result["status"] != "ok" {
		t.Errorf("status = %v, want ok", result["status"])
	}
}

func TestMakeApiRequest_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	var result map[string]any
	err := makeApiRequest(srv.URL, &result)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestMakeApiRequest_HTTPError(t *testing.T) {
	// Use an unreachable URL to trigger an HTTP error
	var result map[string]any
	err := makeApiRequest("http://127.0.0.1:1", &result)
	if err == nil {
		t.Fatal("expected error for connection refused")
	}
}

func TestGetFloat(t *testing.T) {
	if v := getFloat(nil); v != 0.0 {
		t.Errorf("nil: got %v, want 0.0", v)
	}
	if v := getFloat(42.5); v != 42.5 {
		t.Errorf("42.5: got %v", v)
	}
	if v := getFloat(0.0); v != 0.0 {
		t.Errorf("0.0: got %v", v)
	}
}
