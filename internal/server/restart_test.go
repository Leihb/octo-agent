package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleRestart_Returns202AndMarksPending(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	req := httptest.NewRequest(http.MethodPost, "/api/restart", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", w.Code, w.Body.String())
	}
	if !srv.restartPending.Load() {
		t.Error("restartPending = false after POST /api/restart, want true")
	}

	// Join the background shutdown goroutine (Shutdown is single-flight: a
	// second call waits for the first) so it can't race the next test on the
	// process-global tool registries.
	_ = srv.Shutdown(context.Background())
}

func TestRestart_Idempotent(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	srv.Restart("first")
	srv.Restart("second") // must not panic or double-shutdown

	if !srv.restartPending.Load() {
		t.Error("restartPending = false after Restart, want true")
	}
	_ = srv.Shutdown(context.Background())
}

// TestShutdown_SingleFlight runs concurrent Shutdown calls; under -race this
// proves the process-global registry teardown is not executed twice in
// parallel (the Restart-goroutine vs Ctrl-C-handler race).
func TestShutdown_SingleFlight(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	const callers = 4
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { errs <- srv.Shutdown(context.Background()) }()
	}
	for i := 0; i < callers; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("Shutdown returned %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Shutdown calls did not all return")
		}
	}
}

// TestListenAndServe_RestartReturnsSentinel binds a real listener on an
// ephemeral port, requests a restart, and verifies ListenAndServe comes back
// with ErrRestartRequested rather than the plain http.ErrServerClosed.
func TestListenAndServe_RestartReturnsSentinel(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveOn(ln) }()

	srv.Restart("test")

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRestartRequested) {
			t.Fatalf("serveOn returned %v, want ErrRestartRequested", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn did not return within 5s of Restart")
	}
}

// TestListenAndServe_PlainShutdownIsClean verifies a non-restart Shutdown
// (the Ctrl-C path) does NOT surface as a restart request or an error.
func TestListenAndServe_PlainShutdownIsClean(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveOn(ln) }()

	// Give Serve a beat to start accepting, then shut down normally.
	time.Sleep(50 * time.Millisecond)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveOn returned %v after plain Shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn did not return within 5s of Shutdown")
	}
}
