package memory

import (
	"strings"
	"testing"
)

func TestInjector_StartupInjection(t *testing.T) {
	content := `## 🔴 CRITICAL — Worktree
确认话术：已确认 worktree。

规范内容。

## 🔴 CRITICAL — 测试要求
确认话术：已确认测试。

测试规范内容。
`
	parsed := parseStructured(content)
	inj := NewInjector(parsed)

	// Initially has critical entries
	if !inj.HasCritical() {
		t.Fatal("expected HasCritical=true initially")
	}

	// Startup injection should include both
	startup := inj.StartupInjection()
	if !strings.Contains(startup, "Worktree") {
		t.Errorf("startup missing Worktree: %s", startup)
	}
	if !strings.Contains(startup, "测试要求") {
		t.Errorf("startup missing 测试要求: %s", startup)
	}
	if !strings.Contains(startup, "已确认 worktree") {
		t.Errorf("startup missing confirm text: %s", startup)
	}

	// Acknowledge one
	inj.Acknowledge("Worktree")
	if !inj.HasCritical() {
		t.Fatal("expected HasCritical=true after partial acknowledge")
	}

	startup = inj.StartupInjection()
	if strings.Contains(startup, "Worktree") {
		t.Errorf("acknowledged entry should not appear: %s", startup)
	}
	if !strings.Contains(startup, "测试要求") {
		t.Errorf("unacknowledged entry should still appear: %s", startup)
	}

	// Acknowledge all
	inj.AcknowledgeAll()
	if inj.HasCritical() {
		t.Fatal("expected HasCritical=false after AcknowledgeAll")
	}
	if inj.StartupInjection() != "" {
		t.Errorf("expected empty startup after all acknowledged")
	}
}

func TestInjector_StartupInjection_NoCritical(t *testing.T) {
	content := `## 🟡 HIGH — Something
## 🟢 NORMAL — Else
`
	parsed := parseStructured(content)
	inj := NewInjector(parsed)

	if inj.HasCritical() {
		t.Fatal("expected no critical entries")
	}
	if inj.StartupInjection() != "" {
		t.Errorf("expected empty startup: %s", inj.StartupInjection())
	}
}

func TestInjector_TriggerRecall(t *testing.T) {
	content := `## 🟡 HIGH — 测试规范
触发场景：跑测试、build、make

make test 必须全绿才能 push。

## 🟡 HIGH — 代码规范
触发场景：改代码、import

新增依赖需说明理由。
`
	parsed := parseStructured(content)
	inj := NewInjector(parsed)

	// First trigger
	recall := inj.TriggerRecall("帮我跑一下测试")
	if !strings.Contains(recall, "测试规范") {
		t.Errorf("recall missing 测试规范: %s", recall)
	}
	if strings.Contains(recall, "代码规范") {
		t.Errorf("recall should not include 代码规范: %s", recall)
	}

	// Same trigger again — should be deduplicated
	recall2 := inj.TriggerRecall("再跑测试")
	if recall2 != "" {
		t.Errorf("duplicate recall should be empty, got: %s", recall2)
	}

	// Different trigger
	recall3 := inj.TriggerRecall("改一下代码")
	if !strings.Contains(recall3, "代码规范") {
		t.Errorf("recall missing 代码规范: %s", recall3)
	}
}

func TestInjector_TriggerRecall_NoMatch(t *testing.T) {
	content := `## 🟡 HIGH — 测试规范
触发场景：跑测试
`
	parsed := parseStructured(content)
	inj := NewInjector(parsed)

	recall := inj.TriggerRecall("完全不相关的内容")
	if recall != "" {
		t.Errorf("expected empty recall, got: %s", recall)
	}
}

func TestExtractKeywords(t *testing.T) {
	input := "帮我跑一下测试，build 项目"
	kw := extractKeywords(input)

	// Should extract meaningful words
	found := make(map[string]bool)
	for _, k := range kw {
		found[k] = true
	}

	if !found["测试"] {
		t.Errorf("expected '测试' in keywords, got: %v", kw)
	}
	if !found["build"] {
		t.Errorf("expected 'build' in keywords, got: %v", kw)
	}
}
