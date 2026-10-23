package app

// Regression tests for the reproduced A1/A2 scheduling invariants.
import (
	"fmt"
	"testing"
	"time"
)

func TestAuditCooldownSurvivesConcurrentSuccess(t *testing.T) {
	a := testApp(t)
	if _, err := a.addKey("tavily", "audit", "audit-key", 1, 2); err != nil {
		t.Fatal(err)
	}
	first, err := a.acquire("tavily", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.acquire("tavily", nil)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute).Unix()
	a.release(first, 429, until, "upstream HTTP 429")
	a.release(second, 200, 0, "")
	keys, err := a.keys("tavily")
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].CooldownUntil < until {
		t.Fatalf("concurrent success erased cooldown: got %d, want >= %d", keys[0].CooldownUntil, until)
	}
}

func TestAuditReplacementIgnoresOldFailure(t *testing.T) {
	a := testApp(t)
	id, err := a.addKey("tavily", "audit", "old-key", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	old, err := a.acquire("tavily", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := call(a, "PUT", fmt.Sprintf("/api/admin/keys/%d", id), a.adminToken,
		`{"enabled":true,"weight":1,"max_concurrent":2,"secret":"new-key"}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	a.release(old, 401, 0, "upstream HTTP 401")
	keys, err := a.keys("tavily")
	if err != nil {
		t.Fatal(err)
	}
	if !keys[0].Enabled {
		t.Fatal("old credential's 401 disabled the replacement credential")
	}
}
