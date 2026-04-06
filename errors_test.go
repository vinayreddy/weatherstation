package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestWrap(t *testing.T) {
	cause := fmt.Errorf("disk full")
	err := Wrap(cause, "write failed")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if got := err.Error(); got != "write failed: disk full" {
		t.Fatalf("unexpected error string: %s", got)
	}
}

func TestWrap_NilErr(t *testing.T) {
	if err := Wrap(nil, "should be nil"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWrap_StackTrace(t *testing.T) {
	cause := fmt.Errorf("boom")
	err := Wrap(cause, "wrapped")
	formatted := fmt.Sprintf("%+v", err)
	if !strings.Contains(formatted, "weatherstation") {
		t.Fatalf("stack trace should contain module name, got:\n%s", formatted)
	}
	if !strings.Contains(formatted, "errors_test.go") {
		t.Fatalf("stack trace should contain test file name, got:\n%s", formatted)
	}
}

func TestWrap_PreservesStack(t *testing.T) {
	inner := Wrap(fmt.Errorf("root"), "inner")
	outer := Wrap(inner, "outer")

	innerWs := inner.(*wsError)
	outerWs := outer.(*wsError)
	if innerWs.stack != outerWs.stack {
		t.Fatal("outer wrap should reuse inner stack")
	}
}

func TestWrapf(t *testing.T) {
	cause := fmt.Errorf("timeout")
	err := Wrapf(cause, "request to %s failed", "api.example.com")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	want := "request to api.example.com failed: timeout"
	if got := err.Error(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestErrorf(t *testing.T) {
	err := Errorf("connection refused on port %d", 8080)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if got := err.Error(); got != "connection refused on port 8080" {
		t.Fatalf("unexpected: %s", got)
	}
	formatted := fmt.Sprintf("%+v", err)
	if !strings.Contains(formatted, "errors_test.go") {
		t.Fatalf("stack trace missing, got:\n%s", formatted)
	}
}

func TestUnwrap(t *testing.T) {
	sentinel := fmt.Errorf("sentinel")
	err := Wrap(sentinel, "layer1")
	err = Wrap(err, "layer2")
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is should find sentinel through wrapped chain")
	}
}
