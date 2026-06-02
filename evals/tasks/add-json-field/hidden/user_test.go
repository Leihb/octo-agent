package fixture

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUserAgeSerialized(t *testing.T) {
	u := User{Name: "Ada", Email: "ada@example.com", Age: 36}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"age":36`) {
		t.Errorf("expected `\"age\":36` in %s", b)
	}
}
