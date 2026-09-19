package platform

import "testing"

func TestTrustAsymmetricPenalty(t *testing.T) {
	tr := NewTrustManager(500)
	tr.Reward(100)
	if got := tr.Score(); got != 600 {
		t.Fatalf("after +100 reward = %v, want 600", got)
	}
	tr.Penalize(100)
	if got := tr.Score(); got != 450 {
		t.Fatalf("after -100 penalty (1.5x) = %v, want 450", got)
	}
}

func TestTrustClampsToBounds(t *testing.T) {
	tr := NewTrustManager(0)
	tr.Penalize(10)
	if got := tr.Score(); got != 0 {
		t.Fatalf("penalty below floor = %v, want 0", got)
	}
	tr2 := NewTrustManager(1000)
	tr2.Reward(10)
	if got := tr2.Score(); got != 1000 {
		t.Fatalf("reward above ceiling = %v, want 1000", got)
	}
}
