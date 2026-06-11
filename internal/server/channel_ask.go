package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/app"
	"github.com/Leihb/octo-agent/internal/channel"
	"github.com/Leihb/octo-agent/internal/tools"
)

// channelAskTimeout bounds how long an IM permission prompt waits for the
// user's reply before denying. Matches the web confirmation modal's posture
// of "no answer = no". Variable, not const, so tests can shorten it.
var channelAskTimeout = 5 * time.Minute

// imAffirmatives are the only replies that approve an IM permission prompt.
// Anything else denies — over chat, silence and ambiguity must fail closed.
var imAffirmatives = map[string]bool{
	"yes": true, "y": true, "ok": true, "allow": true,
	"是": true, "可以": true, "同意": true, "允许": true,
}

func isAffirmative(text string) bool {
	return imAffirmatives[strings.ToLower(strings.TrimSpace(text))]
}

// askInputSummary renders the part of toolInput the user is actually
// approving. The IM transport shows no tool cards before the gate fires, so
// without this the user would approve blind — "Allow terminal?" tells them
// nothing about WHICH command. Known primary fields come first; anything
// else falls back to a JSON head.
func askInputSummary(toolInput map[string]any) string {
	const maxLen = 160
	for _, key := range []string{"command", "path", "url", "reason", "pattern"} {
		if v, ok := toolInput[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncateForAsk(v, maxLen)
		}
	}
	if len(toolInput) == 0 {
		return ""
	}
	b, err := json.Marshal(toolInput)
	if err != nil {
		return ""
	}
	return truncateForAsk(string(b), maxLen)
}

func truncateForAsk(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// channelPermissionAsk builds the app.PermissionAsk for one IM turn: it sends
// a confirmation prompt into the chat and consumes the requesting user's next
// plain message in that chat as the answer (routed by the inbound dispatcher
// via DeliverAskReply, ahead of the turn path — see routeChannelEvent).
// Approval requires an explicit affirmative; any other reply, the turn being
// cancelled (/stop), or the timeout denies. remember is always false: chat
// approvals are one-shot, a lingering allow in a group chat would outlive
// the person who granted it.
func (s *Server) channelPermissionAsk(sess *channel.Session, ad channel.Adapter, ev channel.InboundEvent) app.PermissionAsk {
	return func(ctx context.Context, toolName string, toolInput map[string]any) (bool, bool, error) {
		replyCh, release, err := sess.BeginAsk(ev.ChatID, ev.UserID)
		if err != nil {
			return false, false, err
		}
		defer release()

		what := toolName
		if detail := askInputSummary(toolInput); detail != "" {
			what = fmt.Sprintf("%s — %q", toolName, detail)
		}
		prompt := fmt.Sprintf(
			"⚠️ Allow %s? Reply yes / 允许 to approve — any other reply denies; only the requester's reply counts. (Auto-deny in %s; /stop cancels the task.)",
			what, channelAskTimeout)
		ad.SendText(ev.ChatID, prompt, ev.MessageID)

		timer := time.NewTimer(channelAskTimeout)
		defer timer.Stop()
		select {
		case text := <-replyCh:
			return isAffirmative(text), false, nil
		case <-ctx.Done():
			return false, false, ctx.Err()
		case <-timer.C:
			return false, false, nil
		}
	}
}

// channelAsker adapts the chat into a tools.Asker for ask_user_question:
// the question goes out as a numbered list, and the requesting user's next
// message answers it through the same session ask slot the permission
// prompt uses. Stamped into the turn ctx by handleChannelMessage
// (tools.WithAsker), where it overrides the process-global wsAsker — which
// would otherwise broadcast IM questions to browser tabs that don't exist.
func (s *Server) channelAsker(sess *channel.Session, ad channel.Adapter, ev channel.InboundEvent) tools.Asker {
	return chatAsker{sess: sess, ad: ad, ev: ev}
}

type chatAsker struct {
	sess *channel.Session
	ad   channel.Adapter
	ev   channel.InboundEvent
}

func (c chatAsker) Ask(ctx context.Context, q tools.AskRequest) (tools.AskResponse, error) {
	replyCh, release, err := c.sess.BeginAsk(c.ev.ChatID, c.ev.UserID)
	if err != nil {
		return tools.AskResponse{}, err
	}
	defer release()

	var b strings.Builder
	b.WriteString("❓ ")
	if q.Header != "" {
		fmt.Fprintf(&b, "[%s] ", q.Header)
	}
	b.WriteString(q.Question + "\n")
	for i, opt := range q.Options {
		fmt.Fprintf(&b, "%d. %s\n", i+1, opt)
	}
	if q.MultiSelect {
		b.WriteString("Reply with number(s), e.g. 1,3 — or free text for something else.")
	} else {
		b.WriteString("Reply with a number — or free text for something else.")
	}
	c.ad.SendText(c.ev.ChatID, b.String(), c.ev.MessageID)

	timer := time.NewTimer(channelAskTimeout)
	defer timer.Stop()
	select {
	case text := <-replyCh:
		return parseAskReply(text, q), nil
	case <-ctx.Done():
		return tools.AskResponse{Cancelled: true}, ctx.Err()
	case <-timer.C:
		return tools.AskResponse{Cancelled: true}, nil
	}
}

// parseAskReply maps the chat reply onto the structured response: numbers
// pick options (several for multi-select), an exact label matches its
// option, anything else is a free-text "Other" answer. Out-of-range numbers
// fall through to free text rather than erroring — over chat, re-prompting
// loops are worse than letting the model see the raw reply.
func parseAskReply(text string, q tools.AskRequest) tools.AskResponse {
	t := strings.TrimSpace(text)
	if t == "" {
		return tools.AskResponse{Cancelled: true}
	}

	parts := strings.FieldsFunc(t, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ' '
	})
	var choices []string
	numeric := len(parts) > 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > len(q.Options) {
			numeric = false
			break
		}
		choices = append(choices, q.Options[n-1])
	}
	if numeric {
		if !q.MultiSelect && len(choices) > 1 {
			choices = choices[:1]
		}
		return tools.AskResponse{Choices: choices}
	}

	for _, opt := range q.Options {
		if strings.EqualFold(t, opt) {
			return tools.AskResponse{Choices: []string{opt}}
		}
	}
	return tools.AskResponse{Custom: t}
}
