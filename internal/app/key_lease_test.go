package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func leaseKey(t *testing.T, a *App) Key {
	t.Helper()
	k, err := a.acquire("tavily", nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func onlyKey(t *testing.T, a *App) Key {
	t.Helper()
	ks, err := a.keys("tavily")
	if err != nil || len(ks) != 1 {
		t.Fatalf("keys: %v, count %d", err, len(ks))
	}
	return ks[0]
}
func TestLeaseCooldownCompletionOrders(t *testing.T) {
	for _, order := range []string{"success-first", "failure-first", "shorter-last", "longer-last"} {
		t.Run(order, func(t *testing.T) {
			a := testApp(t)
			if _, err := a.addKey("tavily", "test", "fake", 1, 2); err != nil {
				t.Fatal(err)
			}
			first, second := leaseKey(t, a), leaseKey(t, a)
			until := time.Now().Add(time.Hour).Unix()
			switch order {
			case "success-first":
				a.release(first, 200, 0, "")
				a.release(second, 429, until, "long")
			case "failure-first":
				a.release(first, 429, until, "long")
				a.release(second, 200, 0, "")
			case "shorter-last":
				a.release(first, 429, until, "long")
				a.release(second, 500, until-100, "short")
			case "longer-last":
				a.release(first, 500, until-100, "short")
				a.release(second, 429, until, "long")
			}
			k := onlyKey(t, a)
			if k.CooldownUntil != until || k.LastError != "long" {
				t.Fatalf("lost cooldown/diagnostic: %+v", k)
			}
			if _, err := a.acquire("tavily", nil); err == nil {
				t.Fatal("cooling key scheduled")
			}
			a.release(first, 200, 0, "") // duplicate release must be harmless
			if a.flight[k.ID] != 0 {
				t.Fatal("slot accounting corrupted")
			}
		})
	}
}

func TestLeaseAdministrativeFences(t *testing.T) {
	for _, mutation := range []string{"replace", "reset", "disable", "toggle", "metadata", "delete"} {
		for _, status := range []int{200, 401, 429} {
			t.Run(fmt.Sprintf("%s/%d", mutation, status), func(t *testing.T) {
				a := testApp(t)
				id, err := a.addKey("tavily", "test", "old-fake", 1, 1)
				if err != nil {
					t.Fatal(err)
				}
				old := leaseKey(t, a)
				body := `{"enabled":true,"weight":1,"max_concurrent":1`
				switch mutation {
				case "replace":
					body += `,"secret":"new-fake"`
				case "reset":
					body += `,"reset_cooldown":true`
				case "disable", "toggle":
					body = `{"enabled":false,"weight":1,"max_concurrent":1`
				case "metadata":
					body += `,"label":"renamed"`
				}
				path := fmt.Sprintf("/api/admin/keys/%d", id)
				method := "PUT"
				if mutation == "delete" {
					method = "DELETE"
				}
				if w := call(a, method, path, a.adminToken, body+`}`); w.Code != 200 {
					t.Fatal(w.Body)
				}
				if mutation == "toggle" {
					if w := call(a, "PUT", path, a.adminToken, `{"enabled":true,"weight":1,"max_concurrent":1}`); w.Code != 200 {
						t.Fatal(w.Body)
					}
				}
				if _, err := a.acquire("tavily", nil); err == nil {
					t.Fatal("old generation stopped consuming slot")
				}
				a.release(old, status, time.Now().Add(time.Minute).Unix(), "old failure")
				a.release(old, status, 0, "duplicate")
				if a.flight[id] != 0 {
					t.Fatal("leaked or negative occupancy")
				}
				if mutation == "delete" {
					return
				}
				k := onlyKey(t, a)
				if mutation == "metadata" {
					if k.revision != old.revision {
						t.Fatal("metadata changed revision")
					}
					if status == 401 && k.Enabled {
						t.Fatal("current lease failure ignored")
					}
					return
				}
				if k.revision <= old.revision || k.Enabled != (mutation != "disable") || k.CooldownUntil != 0 || k.LastError != "" || k.Successes != 0 {
					t.Fatalf("stale release changed state: %+v", k)
				}
				if mutation != "disable" {
					fresh := leaseKey(t, a)
					a.release(fresh, 200, 0, "")
				}
			})
		}
	}
}

func TestManualKeyLeaseFencesAndConcurrency(t *testing.T) {
	for _, status := range []int{200, 401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			a := testApp(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			setupTavily(t, a, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-resume
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, validResult)
				}
			})
			defer unblock()
			id, err := a.addKey("tavily", "manual", "fake-old", 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := a.testKey(context.Background(), id); done <- err }()
			<-entered
			if _, err := a.testKey(context.Background(), id); err == nil {
				t.Fatal("manual concurrency exceeded")
			}
			if _, err := a.acquire("tavily", nil); err == nil {
				t.Fatal("search ignored manual occupancy")
			}
			w := call(a, "PUT", fmt.Sprintf("/api/admin/keys/%d", id), a.adminToken, `{"enabled":true,"weight":1,"max_concurrent":1,"secret":"fake-new"}`)
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			k := onlyKey(t, a)
			if !k.Enabled || k.CooldownUntil != 0 || k.LastError != "" || k.Successes != 0 || a.flight[id] != 0 {
				t.Fatalf("manual stale lease: %+v", k)
			}
		})
	}
}

func TestKeyRevisionLegacyMigrationAndRestart(t *testing.T) {
	a := testApp(t)
	id, err := a.addKey("tavily", "preserved", "fake-secret", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Unix()
	if _, err = a.db.Exec(`UPDATE api_keys SET enabled=0,cooldown_until=?,last_error='preserved',uses=7,successes=5 WHERE id=?`, until, id); err != nil {
		t.Fatal(err)
	}
	// Recreate the exact pre-revision schema, preserving encrypted material.
	_, err = a.db.Exec(`CREATE TABLE legacy_keys(id INTEGER PRIMARY KEY AUTOINCREMENT,provider_id TEXT NOT NULL,label TEXT NOT NULL,secret TEXT NOT NULL,masked TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,weight INTEGER NOT NULL DEFAULT 1,max_concurrent INTEGER NOT NULL DEFAULT 2,cooldown_until INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',uses INTEGER NOT NULL DEFAULT 0,successes INTEGER NOT NULL DEFAULT 0);
 INSERT INTO legacy_keys SELECT id,provider_id,label,secret,masked,enabled,weight,max_concurrent,cooldown_until,last_error,uses,successes FROM api_keys;
 DROP TABLE api_keys; ALTER TABLE legacy_keys RENAME TO api_keys;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		restored, err := New(a.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		k := onlyKey(t, restored)
		plain, err := restored.decrypt(k.secret)
		if err != nil || plain != "fake-secret" || k.ID != id || k.revision != int64(i+1) || k.Enabled || k.Weight != 3 || k.MaxConcurrent != 4 || k.Uses != 7 || k.Successes != 5 || k.CooldownUntil != until || k.LastError != "preserved" {
			t.Fatalf("migration lost state: %+v, %v", k, err)
		}
		if i == 0 {
			if _, err := restored.db.Exec(`UPDATE api_keys SET revision=revision+1 WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
		if err = restored.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
