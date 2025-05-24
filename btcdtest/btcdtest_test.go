package btcdtest

import (
	"testing"
)

func TestFail(t *testing.T) {
	for i := 0; i < 100; i++ {
		t.Logf("start: %d\n", i)

		btcd := New()
		btcd.Stop()

		t.Logf("stop: %d\n", i)
	}
}
