//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"uni-agent-search/internal/app"
)

func TestServerSIGTERMLifecycle(t *testing.T) {
	t.Setenv("SAS_ADMIN_TOKEN", "test-admin-token-not-a-real-secret")
	t.Setenv("SAS_CLIENT_TOKEN", "test-client-token-not-a-real-secret")
	binary := filepath.Join(t.TempDir(), "server")
	build := exec.Command("go", "build", "-race", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, mode := range []string{"idle", "drain", "timeout", "listen-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			a, err := app.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			creds := struct{ Admin, Client string }{"test-admin-token-not-a-real-secret", "test-client-token-not-a-real-secret"}
			entered, cancelled, resume := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Consume POST data so net/http can observe peer disconnects.
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-resume:
					fmt.Fprint(w, `{"results":[{"title":"test","url":"https://example.com/","content":"test"}]}`)
				case <-r.Context().Done():
					close(cancelled)
				}
			}))
			defer upstream.Close()
			defer unblock()
			// Configure through the same API as administrators, entirely offline.
			for _, id := range []string{"bing", "duckduckgo", "tavily"} {
				body := fmt.Sprintf(`{"name":"test","base_url":%q,"enabled":%t,"timeout_seconds":75}`, upstream.URL, id == "tavily")
				req := httptest.NewRequest("PUT", "/api/admin/providers/"+id, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+creds.Admin)
				w := httptest.NewRecorder()
				a.Handler().ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			req := httptest.NewRequest("POST", "/api/admin/keys", strings.NewReader(`{"provider_id":"tavily","secret":"fake-test-key","max_concurrent":1}`))
			req.Header.Set("Authorization", "Bearer "+creds.Admin)
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, req)
			if w.Code != 201 {
				t.Fatal(w.Body)
			}
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			if mode != "listen-failure" {
				listener.Close()
			} else {
				defer listener.Close()
			}
			cmd := exec.Command(binary)
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "SAS_DATA_DIR=") && !strings.HasPrefix(v, "SAS_LISTEN=") {
					cmd.Env = append(cmd.Env, v)
				}
			}
			cmd.Env = append(cmd.Env, "SAS_DATA_DIR="+dir, "SAS_LISTEN="+addr)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			reaped := false
			defer func() {
				if !reaped {
					_ = cmd.Process.Kill()
					<-exited
				}
			}()
			waitExit := func() error {
				t.Helper()
				select {
				case err := <-exited:
					reaped = true
					return err
				case <-time.After(25 * time.Second):
					t.Fatal("server failed to exit within budget")
					return nil
				}
			}
			if mode == "listen-failure" {
				if err := waitExit(); err == nil {
					t.Fatal("bind failure returned success")
				}
				return
			}
			httpClient := &http.Client{Timeout: 2 * time.Second}
			defer httpClient.CloseIdleConnections()
			deadline := time.Now().Add(10 * time.Second)
			for {
				res, err := httpClient.Get("http://" + addr + "/healthz")
				if err == nil {
					res.Body.Close()
					if res.StatusCode == 200 {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("server did not become healthy")
				}
				time.Sleep(20 * time.Millisecond)
			}
			response := make(chan error, 1)
			if mode != "idle" {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
					defer cancel()
					req, err := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/v1/search", strings.NewReader(`{"query":"shutdown regression"}`))
					if err != nil {
						response <- err
						return
					}
					req.Header.Set("Authorization", "Bearer "+creds.Client)
					res, err := http.DefaultClient.Do(req)
					if err == nil {
						defer res.Body.Close()
						_, err = io.Copy(io.Discard, res.Body)
						if res.StatusCode != 200 {
							err = fmt.Errorf("HTTP %d", res.StatusCode)
						}
					}
					response <- err
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("request never reached upstream")
				}
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if mode == "drain" {
				// The child must still be alive with the admitted request blocked.
				select {
				case err := <-exited:
					reaped = true
					t.Fatalf("exited before drain: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
				unblock()
				if err := <-response; err != nil {
					t.Fatalf("drained request failed: %v", err)
				}
			}
			if err := waitExit(); err != nil {
				t.Fatalf("exit: %v %s", err, output.String())
			}
			if mode == "timeout" {
				select {
				case <-cancelled:
				case <-time.After(2 * time.Second):
					t.Fatal("upstream was not cancelled")
				}
				<-response
				if !strings.Contains(output.String(), "cancelling active requests") {
					t.Fatal("timeout path not exercised")
				}
			}
			if strings.Contains(output.String(), "DATA RACE") {
				t.Fatal(output.String())
			}
			// The request log is written at the end of search. Its persistence proves
			// handlers reached their DB tail before shutdown released SQLite.
			restored, err := app.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			req = httptest.NewRequest("GET", "/api/admin/logs", nil)
			req.Header.Set("Authorization", "Bearer "+creds.Admin)
			w = httptest.NewRecorder()
			restored.Handler().ServeHTTP(w, req)
			var logs []map[string]any
			if err = json.Unmarshal(w.Body.Bytes(), &logs); err != nil {
				t.Fatal(err)
			}
			expected := 0
			if mode != "idle" {
				expected = 1
			}
			if len(logs) != expected {
				t.Fatalf("handler DB tail missing: %s", w.Body)
			}
			if mode == "drain" && logs[0]["ok"] != true {
				t.Fatal("drained request not successful")
			}
			if mode == "timeout" && logs[0]["ok"] != false {
				t.Fatal("cancelled request not recorded as failure")
			}
		})
	}
}
