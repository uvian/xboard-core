package traffic

import "testing"

// Drain 必须"取走即清零"：重复上报同一批流量是面板侧最常见的资费事故。
func TestDrainSwapsOut(t *testing.T) {
	c := NewCollector()
	c.Add(1, 100, 200)
	c.Add(1, 1, 2)
	c.Add(2, 0, 5)

	got := c.Drain()
	if pair, ok := got[1]; !ok || pair[0] != 101 || pair[1] != 202 {
		t.Fatalf("uid1: %+v", got)
	}
	if pair, ok := got[2]; !ok || pair[0] != 0 || pair[1] != 5 {
		t.Fatalf("uid2: %+v", got)
	}
	if again := c.Drain(); len(again) != 0 {
		t.Fatalf("second drain must be empty, got %+v", again)
	}
	if c.Pending() {
		t.Fatal("pending must be false after drain")
	}
}

// 双零不该占用上报带宽。
func TestDrainSkipsZero(t *testing.T) {
	c := NewCollector()
	c.Add(3, 0, 0)
	if got := c.Drain(); len(got) != 0 {
		t.Fatalf("zero pair must be skipped: %+v", got)
	}
}
