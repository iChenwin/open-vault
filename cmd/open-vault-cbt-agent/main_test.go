package main

import (
	"context"
	"strings"
	"testing"
)

func TestAgentRejectsArgumentsAndRelativeConfig(t *testing.T) {
	if err := run(context.Background(), []string{"unexpected"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("unexpected CLI argument accepted: %v", err)
	}
	if err := run(context.Background(), []string{"-config", "relative.json"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative config accepted: %v", err)
	}
}
