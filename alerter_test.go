package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogAlerter_Fire(t *testing.T) {
	al := &LogAlerter{}
	if err := al.Fire("test title", "test msg"); err != nil {
		t.Fatalf("LogAlerter.Fire returned error: %v", err)
	}
}

func TestMailtrapAlerter_Fire(t *testing.T) {
	var gotAuth string
	var gotPayload map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotPayload)
		w.WriteHeader(200)
		w.Write([]byte(`{"success":true,"message_ids":["msg-1"]}`))
	}))
	defer srv.Close()

	old := mailtrapEndpoint
	mailtrapEndpoint = srv.URL
	defer func() { mailtrapEndpoint = old }()

	al := NewMailtrapAlerter("test-token", "from@test.com", "to@test.com")
	if err := al.Fire("alert title", "alert body"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q, want %q", gotAuth, "Bearer test-token")
	}

	from, ok := gotPayload["from"].(map[string]any)
	if !ok {
		t.Fatal("missing from field")
	}
	if from["email"] != "from@test.com" {
		t.Errorf("from email = %v", from["email"])
	}

	to, ok := gotPayload["to"].([]any)
	if !ok || len(to) == 0 {
		t.Fatal("missing to field")
	}
	toMap := to[0].(map[string]any)
	if toMap["email"] != "to@test.com" {
		t.Errorf("to email = %v", toMap["email"])
	}

	if gotPayload["subject"] != "alert title" {
		t.Errorf("subject = %v", gotPayload["subject"])
	}
	if gotPayload["text"] != "alert body" {
		t.Errorf("text = %v", gotPayload["text"])
	}
}

func TestMailtrapAlerter_Fire_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	old := mailtrapEndpoint
	mailtrapEndpoint = srv.URL
	defer func() { mailtrapEndpoint = old }()

	al := NewMailtrapAlerter("tok", "from@t.com", "to@t.com")
	err := al.Fire("title", "body")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code, got: %v", err)
	}
}
