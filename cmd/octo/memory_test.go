package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Leihb/octo-agent/internal/memory"
)

func TestRunMemory_List(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	var out bytes.Buffer
	if code := runMemory([]string{"list"}, &out, &out); code != 0 {
		t.Fatalf("empty list exit = %d", code)
	}
	if !strings.Contains(out.String(), "No memories") {
		t.Errorf("empty store should say so:\n%s", out.String())
	}

	store, err := memory.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(memory.Entry{Name: "n", Description: "prefers Go", Type: memory.TypeUser}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	runMemory([]string{"list"}, &out, &out)
	if !strings.Contains(out.String(), "prefers Go") {
		t.Errorf("list should show the entry:\n%s", out.String())
	}
}

func TestRunMemory_BadSubcommand(t *testing.T) {
	var out bytes.Buffer
	if code := runMemory([]string{"bogus"}, &out, &out); code != 2 {
		t.Errorf("bad subcommand exit = %d, want 2", code)
	}
	if code := runMemory(nil, &out, &out); code != 2 {
		t.Errorf("no subcommand exit = %d, want 2", code)
	}
}

// printMemory backs the TUI's /memory command (dispatchSlash). It's a pure
// io.Writer renderer, so it's tested directly rather than through the REPL.

func TestPrintMemory_ShowsEntries(t *testing.T) {
	store := memory.NewStoreAt(t.TempDir())
	if err := store.Save(memory.Entry{Name: "n", Description: "remembered thing", Type: memory.TypeFeedback}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printMemory(&out, store)
	if !strings.Contains(out.String(), "remembered thing") {
		t.Errorf("printMemory output missing entry:\n%s", out.String())
	}
}

func TestPrintMemory_Disabled(t *testing.T) {
	var out bytes.Buffer
	printMemory(&out, nil) // nil store → memory disabled
	if !strings.Contains(out.String(), "disabled") {
		t.Errorf("expected disabled notice:\n%s", out.String())
	}
}
