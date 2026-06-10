package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/scheduler"
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

// TestRestart_WaitsForInflightTurn: restart must not shut the server down
// while a turn is active; it proceeds as soon as the turn ends.
func TestRestart_WaitsForInflightTurn(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveOn(ln) }()

	if err := srv.drain.begin(); err != nil { // simulate an in-flight turn
		t.Fatal(err)
	}
	srv.Restart("test")

	select {
	case err := <-errCh:
		t.Fatalf("server stopped (%v) while a turn was in flight", err)
	case <-time.After(150 * time.Millisecond):
	}

	srv.drain.end()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRestartRequested) {
			t.Fatalf("serveOn = %v, want ErrRestartRequested", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after the in-flight turn ended")
	}
}

// TestRestart_DrainTimeoutForcesShutdown: a turn that outlives the drain
// timeout must not block the restart forever.
func TestRestart_DrainTimeoutForcesShutdown(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.drainTimeout = 100 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveOn(ln) }()

	if err := srv.drain.begin(); err != nil {
		t.Fatal(err)
	}
	defer srv.drain.end() // never ends within the timeout

	srv.Restart("test")

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRestartRequested) {
			t.Fatalf("serveOn = %v, want ErrRestartRequested", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not force shutdown after drain timeout")
	}
}

// TestHandleTurn_DrainingReturns503: new turn requests during a drain get a
// retryable 503 instead of starting work that the shutdown would cut short.
func TestHandleTurn_DrainingReturns503(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatal(err)
	}

	srv.drain.drain(0)

	body := bytes.NewBufferString(`{"message":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat/"+sess.ID+"/turn", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", w.Code, w.Body.String())
	}
}

// TestRunTask_DrainingRefused: scheduled task runs are refused during drain.
func TestRunTask_DrainingRefused(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.drain.drain(0)

	_, err := srv.RunTask(context.Background(), scheduler.Task{Name: "t", Prompt: "p"})
	if !errors.Is(err, errDraining) {
		t.Fatalf("RunTask during drain = %v, want errDraining", err)
	}
}

// drainTestAdapter records SendText calls; every other Adapter method is
// unused on the draining path and inherited from the embedded nil interface
// (calling one would panic, which is what we want in this test).
type drainTestAdapter struct {
	channel.Adapter
	sent []string
}

func (a *drainTestAdapter) SendText(chatID, text, replyTo string) channel.SendResult {
	a.sent = append(a.sent, text)
	return channel.SendResult{}
}

// TestHandleChannelMessage_DrainingRepliesPolitely pins the design deviation:
// IM adapters stay up through the drain, and a message arriving mid-drain
// gets an explicit "try again" reply instead of being dropped silently.
func TestHandleChannelMessage_DrainingRepliesPolitely(t *testing.T) {
	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.drain.drain(0)

	ad := &drainTestAdapter{}
	srv.handleChannelMessage(context.Background(), ad, channel.InboundEvent{ChatID: "c1", Text: "hi"})

	if len(ad.sent) != 1 {
		t.Fatalf("SendText calls = %d, want 1", len(ad.sent))
	}
	if !strings.Contains(ad.sent[0], "restarting") {
		t.Errorf("reply %q should mention the restart", ad.sent[0])
	}
}
