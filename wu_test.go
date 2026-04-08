package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWUClient_FetchCurrent(t *testing.T) {
	// Mock WU API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stationId") != "KTEST1" {
			t.Errorf("stationId = %q", r.URL.Query().Get("stationId"))
		}
		if r.URL.Query().Get("apiKey") != "test-key" {
			t.Errorf("apiKey = %q", r.URL.Query().Get("apiKey"))
		}
		resp := map[string]any{
			"observations": []map[string]any{{
				"stationID":      "KTEST1",
				"epoch":          json.Number("1712345678"),
				"humidity":       json.Number("65"),
				"winddir":        json.Number("180"),
				"solarRadiation": json.Number("150"),
				"uv":             json.Number("3.5"),
				"imperial": map[string]any{
					"temp":        json.Number("55.5"),
					"heatIndex":   json.Number("55.5"),
					"dewpt":       json.Number("42.3"),
					"windSpeed":   json.Number("8.2"),
					"windGust":    json.Number("15.1"),
					"pressure":    json.Number("30.12"),
					"precipRate":  json.Number("0.01"),
					"precipTotal": json.Number("0.25"),
				},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	old := wuBaseURL
	wuBaseURL = srv.URL
	defer func() { wuBaseURL = old }()

	wu := NewWUClient("test-key", "KTEST1")
	obs, err := wu.FetchCurrent()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if obs.Temp != 55.5 {
		t.Errorf("temp = %v, want 55.5", obs.Temp)
	}
	if obs.Humidity != 65 {
		t.Errorf("humidity = %v, want 65", obs.Humidity)
	}
	if obs.WindSpeed != 8.2 {
		t.Errorf("windSpeed = %v, want 8.2", obs.WindSpeed)
	}
	if obs.WindDir != 180 {
		t.Errorf("windDir = %v, want 180", obs.WindDir)
	}
	if obs.Pressure != 30.12 {
		t.Errorf("pressure = %v, want 30.12", obs.Pressure)
	}
}

func TestWUClient_FetchCurrent_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"observations": []any{}})
	}))
	defer srv.Close()

	old := wuBaseURL
	wuBaseURL = srv.URL
	defer func() { wuBaseURL = old }()

	wu := NewWUClient("key", "KTEST")
	_, err := wu.FetchCurrent()
	if err == nil {
		t.Fatal("expected error for empty observations")
	}
}

func TestWUClient_FetchCurrent_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	old := wuBaseURL
	wuBaseURL = srv.URL
	defer func() { wuBaseURL = old }()

	wu := NewWUClient("key", "KTEST")
	_, err := wu.FetchCurrent()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestWUClient_FetchHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("date") != "20260406" {
			t.Errorf("date = %q", r.URL.Query().Get("date"))
		}
		resp := map[string]any{
			"observations": []map[string]any{
				{"epoch": json.Number("1712345000"), "humidityAvg": json.Number("60"), "winddirAvg": json.Number("180"),
					"solarRadiationHigh": json.Number("100"), "uvHigh": json.Number("2"),
					"imperial": map[string]any{"tempAvg": json.Number("50"), "windspeedAvg": json.Number("5"),
						"windgustAvg": json.Number("12"), "dewptAvg": json.Number("40"), "pressureMax": json.Number("30.1"),
						"precipRate": json.Number("0"), "precipTotal": json.Number("0")}},
				{"epoch": json.Number("1712345300"), "humidityAvg": json.Number("62"), "winddirAvg": json.Number("190"),
					"imperial": map[string]any{"tempAvg": json.Number("52"), "windspeedAvg": json.Number("6")}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	old := wuBaseURL
	wuBaseURL = srv.URL
	defer func() { wuBaseURL = old }()

	wu := NewWUClient("key", "KTEST")
	obs, err := wu.FetchHistory("20260406")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("got %d observations, want 2", len(obs))
	}
	if obs[0].Temp != 50 {
		t.Errorf("first obs temp = %v, want 50", obs[0].Temp)
	}
	if obs[0].Humidity != 60 {
		t.Errorf("first obs humidity = %v, want 60", obs[0].Humidity)
	}
	if obs[0].WindSpeed != 5 {
		t.Errorf("first obs windSpeed = %v, want 5", obs[0].WindSpeed)
	}
	if obs[0].WindDir != 180 {
		t.Errorf("first obs windDir = %v, want 180", obs[0].WindDir)
	}
	if obs[0].Pressure != 30.1 {
		t.Errorf("first obs pressure = %v, want 30.1", obs[0].Pressure)
	}
	if obs[1].Temp != 52 {
		t.Errorf("second obs temp = %v, want 52", obs[1].Temp)
	}
}
