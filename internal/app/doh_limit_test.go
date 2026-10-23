package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// An oversized malformed payload is insufficient: JSON parsing alone rejects
// it even when the response limit is accidentally relaxed. Keep this valid JSON
// at both sides of the byte boundary to isolate the size-limit mechanism.
func TestDoHRejectsOversizedValidJSON(t *testing.T) {
	const limit = 64 << 10
	const prefix = `{"Status":0,"Answer":[{"type":1,"data":"8.8.8.8"}],"padding":"`
	const suffix = `"}`
	for _, size := range []int{limit, limit + 1} {
		name := "at_limit"
		if size > limit {
			name = "over_limit"
		}
		t.Run(name, func(t *testing.T) {
			body := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			answer, err := resolveDoH(context.Background(), "example.test", "https://doh.invalid/query", client)
			if size == limit {
				if err != nil || len(answer) != 1 {
					t.Fatalf("valid JSON exactly at limit rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("valid JSON above limit was not rejected by the size guard: %v", err)
			}
		})
	}
}
