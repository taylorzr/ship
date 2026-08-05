package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zach/ship/internal/k8s"
)

func TestEventReasonAge(t *testing.T) {
	if reason, age, ok := eventReasonAge("Unhealthy"); ok || age != 0 || reason != "Unhealthy" {
		t.Fatalf("plain entry = (%q, %v, %v), want (Unhealthy, 0, false)", reason, age, ok)
	}

	ts := time.Now().Add(-15 * time.Minute).Unix()
	entry := "Unhealthy@" + strconv.FormatInt(ts, 10)
	reason, age, ok := eventReasonAge(entry)
	if !ok {
		t.Fatalf("encoded entry %q: ok = false", entry)
	}
	if reason != "Unhealthy" {
		t.Fatalf("reason = %q, want Unhealthy", reason)
	}
	if age < 14*time.Minute || age > 16*time.Minute {
		t.Fatalf("age = %v, want ~15m", age)
	}

	if _, _, ok := eventReasonAge("BackOff@notanumber"); ok {
		t.Fatal("malformed timestamp accepted")
	}
}

func TestEventReasonAgeEmitsAgedSegment(t *testing.T) {
	ts := time.Now().Add(-15 * time.Minute).Unix()
	h := k8s.Health{OldEvents: []string{"Unhealthy@" + strconv.FormatInt(ts, 10)}}
	segs := healthSegments(h, eventFilter{})
	found := false
	for _, s := range segs {
		if strings.HasPrefix(s.text, "⚠Unhealthy (-") && strings.HasSuffix(s.text, "m)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("segments = %v, want an aged ⚠Unhealthy segment", segs)
	}
}

func TestHealthRoundTripPreservesEventAges(t *testing.T) {
	h := k8s.Health{
		Ready:         true,
		Replicas:      2,
		ReadyReplicas: 2,
		Events:        []string{"BackOff@123456"},
		OldEvents:     []string{"Unhealthy@654321"},
	}
	s := serializeHealth(h)
	got := parseHealth(s)
	if len(got.Events) != 1 || got.Events[0] != "BackOff@123456" {
		t.Fatalf("Events after round-trip = %v", got.Events)
	}
	if len(got.OldEvents) != 1 || got.OldEvents[0] != "Unhealthy@654321" {
		t.Fatalf("OldEvents after round-trip = %v", got.OldEvents)
	}
}
