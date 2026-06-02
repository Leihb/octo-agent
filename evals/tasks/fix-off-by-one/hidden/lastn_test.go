package fixture

import (
	"reflect"
	"testing"
)

func TestLastN(t *testing.T) {
	cases := []struct {
		s    []int
		n    int
		want []int
	}{
		{[]int{1, 2, 3, 4, 5}, 3, []int{3, 4, 5}},
		{[]int{1, 2, 3}, 1, []int{3}},
		{[]int{1, 2, 3}, 5, []int{1, 2, 3}},
	}
	for _, c := range cases {
		if got := LastN(c.s, c.n); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LastN(%v, %d) = %v, want %v", c.s, c.n, got, c.want)
		}
	}
}
