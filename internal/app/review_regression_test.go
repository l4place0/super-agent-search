package app

import (
	"fmt"
	"strings"
	"testing"
)

func TestReplacementCredentialUniqueness(t *testing.T) {
	a := testApp(t)
	first, err := a.addKey("tavily", "first", "fake-first", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.addKey("tavily", "second", "fake-second", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id     int64
		secret string
		want   int
	}{
		{second, "fake-first", 400},
		{first, "fake-first", 200},
		{second, "fake-new", 200},
	} {
		w := call(a, "PUT", fmt.Sprintf("/api/admin/keys/%d", tc.id), a.adminToken, fmt.Sprintf(`{"enabled":true,"weight":1,"max_concurrent":1,"secret":%q}`, tc.secret))
		if w.Code != tc.want {
			t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body)
		}
		ks, err := a.keys("tavily")
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range ks {
			if k.ID == second {
				plain, err := a.decrypt(k.secret)
				if err != nil {
					t.Fatal(err)
				}
				want := "fake-second"
				if tc.secret == "fake-new" {
					want = "fake-new"
				}
				if plain != want {
					t.Fatalf("replacement not atomic: %q", plain)
				}
			}
		}
	}
	// Uniqueness is scoped to the provider, as it is for additions.
	other, err := a.addKey("brave", "other", "fake-other", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	w := call(a, "PUT", fmt.Sprintf("/api/admin/keys/%d", other), a.adminToken, `{"enabled":true,"weight":1,"max_concurrent":1,"secret":"fake-first"}`)
	if w.Code != 200 {
		t.Fatalf("cross-provider replacement: %d", w.Code)
	}
}

func TestReadHTMLHiddenAttributes(t *testing.T) {
	for _, attribute := range []string{`hidden`, `hidden=""`, `hidden="hidden"`, `hidden="false"`, `aria-hidden="true"`} {
		text, _ := readHTML(`<p>Visible text</p><div ` + attribute + `><p>Hidden text</p></div>`)
		if strings.Contains(text, "Hidden text") || !strings.Contains(text, "Visible text") {
			t.Fatalf("%s: %q", attribute, text)
		}
	}
}
