package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestProviderOutcomePolicy(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		err      error
		status   int
		cooldown int64
		retry    bool
	}{
		{nil, 200, 0, false},
		{errors.New("parse failure"), 0, now.Unix() + 10, false},
		{fmt.Errorf("wrapped: %w", upstreamError{401, 0}), 401, 0, true},
		{upstreamError{403, 0}, 403, 0, true},
		{upstreamError{402, now.Unix() + 60}, 402, now.Unix() + 60, true},
		{upstreamError{429, now.Unix() + 60}, 429, now.Unix() + 60, true},
		{upstreamError{432, now.Unix() + 60}, 432, now.Unix() + 60, true},
		{upstreamError{433, now.Unix() + 60}, 433, now.Unix() + 60, true},
		{upstreamError{400, 0}, 400, 0, false},
		{upstreamError{500, 0}, 500, now.Unix() + 10, false},
		{upstreamError{}, 0, now.Unix() + 10, false},
	} {
		out := classifyProviderOutcome(tc.err, now)
		if out.status != tc.status || out.cooldown != tc.cooldown || out.retry != tc.retry {
			t.Fatalf("%v: %+v", tc.err, out)
		}
		if (tc.err == nil) != (out.reason == "") {
			t.Fatal("missing failure diagnosis")
		}
	}
}

func TestSearchAndManualShareProviderOutcome(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 402, 429, 432, 433, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			states := []Key{}
			for _, manual := range []bool{false, true} {
				a := testApp(t)
				setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(status)
					if status == 200 {
						fmt.Fprint(w, validResult)
					}
				})
				id, err := a.addKey("tavily", "test", "fake", 1, 1)
				if err != nil {
					t.Fatal(err)
				}
				if manual {
					if _, err := a.testKey(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				} else {
					ps, err := a.providers()
					if err != nil {
						t.Fatal(err)
					}
					for _, p := range ps {
						if p.ID == "tavily" {
							a.providerSearch(context.Background(), p, "test", 3)
						}
					}
				}
				states = append(states, onlyKey(t, a))
			}
			first, second := states[0], states[1]
			delta := first.CooldownUntil - second.CooldownUntil
			if delta < 0 {
				delta = -delta
			}
			if first.Enabled != second.Enabled || first.LastError != second.LastError || first.Uses != second.Uses || first.Successes != second.Successes || delta > 1 {
				t.Fatalf("divergent manual/search outcome: %+v / %+v", first, second)
			}
		})
	}
}
