package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Alerter fires alerts (email or log).
type Alerter interface {
	Fire(title, msg string) error
}

// ---------------------------------------------------------------------------
// LogAlerter — logs alerts via slog
// ---------------------------------------------------------------------------

type LogAlerter struct{}

func (a *LogAlerter) Fire(title, msg string) error {
	slog.Error("ALERT", "title", title, "msg", msg)
	return nil
}

// ---------------------------------------------------------------------------
// MailtrapAlerter — sends alerts via the Mailtrap transactional API
// ---------------------------------------------------------------------------

// mailtrapEndpoint is the Mailtrap API URL. Overridden in tests.
var mailtrapEndpoint = "https://send.api.mailtrap.io/api/send"

type MailtrapAlerter struct {
	apiToken string
	from     string
	to       string
	client   *http.Client
}

func NewMailtrapAlerter(apiToken, from, to string) *MailtrapAlerter {
	return &MailtrapAlerter{
		apiToken: apiToken,
		from:     from,
		to:       to,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *MailtrapAlerter) Fire(title, msg string) error {
	payload := map[string]any{
		"from": map[string]string{
			"email": a.from,
			"name":  "Weatherstation",
		},
		"to": []map[string]string{
			{"email": a.to},
		},
		"subject": title,
		"text":    msg,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal alert payload", "err", err)
		return fmt.Errorf("marshal alert payload: %w", err)
	}

	req, err := http.NewRequest("POST", mailtrapEndpoint, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to create alert request", "err", err)
		return fmt.Errorf("create alert request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		slog.Error("failed to send alert", "err", err)
		return fmt.Errorf("send alert: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		err = fmt.Errorf("mailtrap returned status %d: %s", resp.StatusCode, respBody)
		slog.Error("alert send failed", "err", err)
		return err
	}

	return nil
}
