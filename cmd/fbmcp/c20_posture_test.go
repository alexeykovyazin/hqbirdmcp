package main

import (
	"context"
	"strings"
	"testing"
)

func TestC20ServiceControlRefusesWithoutPosture(t *testing.T) {
	gt := newTestGT(t)
	_, err := gt.runServiceControl(context.Background(), "spike5", nil, "start")
	if err == nil || !strings.Contains(err.Error(), "posture") {
		t.Fatalf("expected posture refuse, got %v", err)
	}
}
