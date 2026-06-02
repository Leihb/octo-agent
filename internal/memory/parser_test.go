package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseIndex_StructuredEntries(t *testing.T) {
	content := `# octo-agent 项目记忆

## 🔴 CRITICAL — Git Worktree 模式

**规范**：所有代码修改必须在独立 worktree 中进行。

确认话术：已确认 Git worktree 规范：本次会话的所有代码修改将在独立 worktree 中进行。

触发场景：改代码、git 操作、跑测试

## 🟡 HIGH — 项目架构

- Go 1.22+ 单二进制 CLI
- 模块路径：github.com/Leihb/octo-agent

## 🟢 NORMAL — 用户档案

- Roy，Klook 后端工程师

## 无标签条目

一些普通内容。
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, IndexFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	p := ParseIndex(dir)
	if len(p.Entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(p.Entries))
	}

	// CRITICAL entry
	critical := p.Entries[0]
	if critical.Priority != PriorityCritical {
		t.Errorf("entry 0 priority = %q, want CRITICAL", critical.Priority)
	}
	if critical.Title != "Git Worktree 模式" {
		t.Errorf("entry 0 title = %q, want 'Git Worktree 模式'", critical.Title)
	}
	if !strings.Contains(critical.ConfirmText, "已确认") {
		t.Errorf("entry 0 confirm text missing '已确认': %q", critical.ConfirmText)
	}
	if len(critical.Triggers) != 3 {
		t.Errorf("entry 0 triggers = %v, want 3 items", critical.Triggers)
	}

	// HIGH entry
	high := p.Entries[1]
	if high.Priority != PriorityHigh {
		t.Errorf("entry 1 priority = %q, want HIGH", high.Priority)
	}

	// NORMAL entry
	normal := p.Entries[2]
	if normal.Priority != PriorityNormal {
		t.Errorf("entry 2 priority = %q, want NORMAL", normal.Priority)
	}

	// Unlabeled entry defaults to NORMAL
	unlabeled := p.Entries[3]
	if unlabeled.Priority != PriorityNormal {
		t.Errorf("entry 3 priority = %q, want NORMAL", unlabeled.Priority)
	}
}

func TestParseIndex_MissingFile(t *testing.T) {
	p := ParseIndex(t.TempDir())
	if p.Raw != "" {
		t.Errorf("missing file should yield empty Raw, got %q", p.Raw)
	}
	if len(p.Entries) != 0 {
		t.Errorf("missing file should yield 0 entries, got %d", len(p.Entries))
	}
}

func TestEntriesByPriority(t *testing.T) {
	content := `## 🔴 CRITICAL — A
## 🟡 HIGH — B
## 🔴 CRITICAL — C
## 🟢 NORMAL — D
`
	p := parseStructured(content)
	critical := p.EntriesByPriority(PriorityCritical)
	if len(critical) != 2 {
		t.Fatalf("expected 2 CRITICAL, got %d", len(critical))
	}
	if critical[0].Title != "A" || critical[1].Title != "C" {
		t.Errorf("CRITICAL titles wrong: %v", critical)
	}
}

func TestMatchTriggers(t *testing.T) {
	content := `## 🟡 HIGH — 测试规范
触发场景：跑测试、build、make

要点：make test 必须全绿才能 push。

## 🟡 HIGH — 代码规范
触发场景：改代码、import、加依赖

要点：新增依赖需说明理由。
`
	p := parseStructured(content)

	// Match "测试"
	matched := p.MatchTriggers([]string{"测试"})
	if len(matched) != 1 || matched[0].Title != "测试规范" {
		t.Errorf("match '测试' = %v", matched)
	}

	// Match "build" (should match "build" trigger)
	matched = p.MatchTriggers([]string{"build"})
	if len(matched) != 1 || matched[0].Title != "测试规范" {
		t.Errorf("match 'build' = %v", matched)
	}

	// Match "改代码" (should match 代码规范)
	matched = p.MatchTriggers([]string{"改代码"})
	if len(matched) != 1 || matched[0].Title != "代码规范" {
		t.Errorf("match '改代码' = %v", matched)
	}

	// No match
	matched = p.MatchTriggers([]string{"不存在的词"})
	if len(matched) != 0 {
		t.Errorf("no match expected, got %v", matched)
	}
}

func TestRenderAcknowledgement(t *testing.T) {
	content := `## 🔴 CRITICAL — Worktree
确认话术：已确认 worktree。

规范内容在这里。
`
	p := parseStructured(content)
	ack := p.RenderAcknowledgement()
	if ack == "" {
		t.Fatal("expected non-empty acknowledgement")
	}
	if !strings.Contains(ack, "会话启动确认") {
		t.Errorf("ack missing '会话启动确认': %s", ack)
	}
	if !strings.Contains(ack, "已确认 worktree") {
		t.Errorf("ack missing confirm text: %s", ack)
	}
}

func TestRenderAcknowledgement_NoCritical(t *testing.T) {
	content := `## 🟡 HIGH — Something
## 🟢 NORMAL — Else
`
	p := parseStructured(content)
	ack := p.RenderAcknowledgement()
	if ack != "" {
		t.Errorf("expected empty ack without CRITICAL, got: %s", ack)
	}
}

func TestRenderTriggerRecall(t *testing.T) {
	content := `## 🟡 HIGH — 测试规范
触发场景：跑测试

make test 必须全绿。
`
	p := parseStructured(content)
	recall := p.RenderTriggerRecall([]string{"测试"})
	if recall == "" {
		t.Fatal("expected non-empty recall")
	}
	if !strings.Contains(recall, "测试规范") {
		t.Errorf("recall missing title: %s", recall)
	}
	if !strings.Contains(recall, "make test") {
		t.Errorf("recall missing body: %s", recall)
	}
}

func TestRenderTriggerRecall_NoMatch(t *testing.T) {
	content := `## 🟡 HIGH — 测试规范
触发场景：跑测试
`
	p := parseStructured(content)
	recall := p.RenderTriggerRecall([]string{"完全不相关"})
	if recall != "" {
		t.Errorf("expected empty recall for no match, got: %s", recall)
	}
}

func TestFirstSentence(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Hello world. More text.", "Hello world"},
		{"你好世界。更多内容。", "你好世界"},
		{"No punctuation here", "No punctuation here"},
		{"", ""},
		{"A very long string without any punctuation that exceeds one hundred characters and should be truncated", "A very long string without any punctuation that exceeds one hundred characters and should be truncat..."},
	}
	for _, c := range cases {
		got := firstSentence(c.in)
		if got != c.want {
			t.Errorf("firstSentence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
