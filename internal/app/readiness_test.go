package app

import "testing"

// Keep database availability independent from the sticky KeyPool fault flag.
// Ablation showed the combined storage-failure test could mask a missing probe.
func TestReadinessReportsUnavailableDatabase(t *testing.T) {
	a := testApp(t)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.poolFault.Load() {
		t.Fatal("probe must isolate database failure from KeyPool fault")
	}
	if w := call(a, "GET", "/healthz", "", ""); w.Code != 200 {
		t.Fatal("liveness depends on storage")
	}
	if w := call(a, "GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatalf("closed database reported ready: HTTP %d", w.Code)
	}
}
