package agent

import (
	"sync"
	"testing"
)

// TestAccrueChildUsageConcurrent guards the usage-counter mutex: a fanned-out
// launch_agent batch runs sub-agents concurrently, each folding its tokens into
// the shared parent via AccrueChildUsage. Run with -race, this catches an
// unguarded counter; the sum check catches lost updates.
func TestAccrueChildUsageConcurrent(t *testing.T) {
	a := New(&fakeSender{}, "test-model")

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.AccrueChildUsage(1, 2)
		}()
	}
	wg.Wait()

	in, out := a.SessionTokens()
	if in != n || out != 2*n {
		t.Errorf("SessionTokens = (%d, %d), want (%d, %d)", in, out, n, 2*n)
	}
}
