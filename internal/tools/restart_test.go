package tools

import (
	"context"
	"strings"
	"testing"
)

func defaultToolNames(model string) map[string]bool {
	names := map[string]bool{}
	for _, d := range DefaultToolsFor(model) {
		names[d.Name] = true
	}
	return names
}

func TestRestartServerTool_HiddenWithoutRestarter(t *testing.T) {
	SetRestarter(nil)
	t.Cleanup(func() { SetRestarter(nil) })

	if defaultToolNames("")["restart_server"] {
		t.Error("restart_server advertised with no restarter registered (CLI/TUI mode)")
	}
}

func TestRestartServerTool_AdvertisedWithRestarter(t *testing.T) {
	SetRestarter(func(reason string) {})
	t.Cleanup(func() { SetRestarter(nil) })

	if !defaultToolNames("")["restart_server"] {
		t.Error("restart_server missing from DefaultToolsFor with a restarter registered")
	}
}

func TestRestartServerTool_ExecuteSchedulesRestart(t *testing.T) {
	var gotReason string
	SetRestarter(func(reason string) { gotReason = reason })
	t.Cleanup(func() { SetRestarter(nil) })

	res, err := RestartServerTool{}.Execute(context.Background(), "restart_server",
		map[string]any{"reason": "config change: new model default"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotReason != "config change: new model default" {
		t.Errorf("restarter got reason %q", gotReason)
	}
	if !strings.Contains(res.Text, "after this turn") {
		t.Errorf("result %q should tell the model the restart happens after this turn", res.Text)
	}
}

func TestRestartServerTool_ExecuteWithoutRestarterErrors(t *testing.T) {
	SetRestarter(nil)

	_, err := RestartServerTool{}.Execute(context.Background(), "restart_server", map[string]any{})
	if err == nil {
		t.Fatal("Execute without a restarter should error")
	}
}
