package agent

import "testing"

func TestMessageConstructors(t *testing.T) {
	cases := []struct {
		name    string
		msg     Message
		wantRol Role
	}{
		{"user", NewUserMessage("hi"), RoleUser},
		{"assistant", NewAssistantMessage("hello"), RoleAssistant},
		{"system", NewSystemMessage("you are…"), RoleSystem},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.msg.Role != c.wantRol {
				t.Errorf("Role = %q, want %q", c.msg.Role, c.wantRol)
			}
			if c.msg.Content == "" {
				t.Errorf("Content should not be empty")
			}
		})
	}
}
