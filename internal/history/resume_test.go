package history

import (
	"testing"
	"time"
)

func TestResumeAvailabilityAndIdentity(t *testing.T) {
	r := Record{RunID: "attempt-2", ResumedFrom: "attempt-1", Session: "session", Status: "error",
		ResumeSettings: &ResumeSettings{Version: 1}}
	for _, busy := range []bool{false, true} {
		c := BuildConversations([]Record{r}, map[string]bool{"session": busy}, time.UTC)[0]
		if c.Summary.ResumeAvailable == busy || c.Runs[0].ResumeAvailable == busy {
			t.Fatalf("availability for busy=%v: %+v", busy, c)
		}
		if c.Runs[0].RunID != "attempt-2" || c.Runs[0].ResumedFrom != "attempt-1" {
			t.Fatalf("lost identity: %+v", c)
		}
	}
	r.ResumeSettings = nil
	if BuildConversations([]Record{r}, nil, time.UTC)[0].Summary.ResumeAvailable {
		t.Fatal("legacy run advertised saved resume")
	}
}
