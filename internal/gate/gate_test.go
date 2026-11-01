package gate

import (
	"context"
	"testing"
	"time"
)

// mkUser 造一个面板语义的用户：email 就是内核里的 "uid.uuid"。
func mkUser(uid int, deviceLimit, speedLimit int) User {
	return User{
		UID:         uid,
		Email:       emailOf(uid),
		SpeedLimit:  speedLimit,
		DeviceLimit: deviceLimit,
	}
}

func emailOf(uid int) string {
	return "1." + []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000004",
	}[uid-1]
}

// 第三台设备必须被拒，已登记 IP 重连不受影响。
func TestDeviceLimit(t *testing.T) {
	g := New()
	defer g.Close()
	g.LoadUsers("node-1", []User{mkUser(1, 2, 0)})
	ctx := context.Background()
	email := emailOf(1)

	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		if err := g.Admit(ctx, "node-1", email, ip); err != nil {
			t.Fatalf("%s should pass: %v", ip, err)
		}
	}
	if err := g.Admit(ctx, "node-1", email, "10.0.0.3"); err != ErrDeviceLimit {
		t.Fatalf("third device want ErrDeviceLimit, got %v", err)
	}
	if err := g.Admit(ctx, "node-1", email, "10.0.0.1"); err != nil {
		t.Fatalf("known device should pass: %v", err)
	}
}

// 面板删人后，内核不应等下一轮拉取才拒绝。
func TestUnknownUserRejected(t *testing.T) {
	g := New()
	defer g.Close()
	g.LoadUsers("node-1", []User{mkUser(1, 0, 0)})
	if err := g.Admit(context.Background(), "node-1", "1.nope", "10.0.0.1"); err != ErrUnknownUser {
		t.Fatalf("want ErrUnknownUser, got %v", err)
	}
}

// 节点级与用户级限速取非零最小值，Mbps → 字节/秒。
func TestRateTakesSmallerLimit(t *testing.T) {
	const mbps = 1000 * 1000 / 8
	g := New()
	defer g.Close()
	g.SetNodeLimit("node-1", 100)
	g.LoadUsers("node-1", []User{mkUser(1, 0, 20)})
	if got, want := g.Rate("node-1", emailOf(1)), int64(20*mbps); got != want {
		t.Fatalf("want %d got %d", want, got)
	}
	g.SetNodeLimit("node-1", 5)
	if got, want := g.Rate("node-1", emailOf(1)), int64(5*mbps); got != want {
		t.Fatalf("node limit smaller: want %d got %d", want, got)
	}
}

// 动态限速到期必须失效，否则"限速 1 小时"会变成永久。
func TestDynamicLimitExpires(t *testing.T) {
	g := New()
	defer g.Close()
	g.LoadUsers("node-1", []User{mkUser(1, 0, 0)})
	g.SetDynamicLimit("node-1", emailOf(1), 1, time.Now().Add(-time.Second))
	if got := g.Rate("node-1", emailOf(1)); got != 0 {
		t.Fatalf("expired dynamic limit still active: %d", got)
	}
}

// 面板改 device_limit 必须即时生效。
func TestUpdateUserLimit(t *testing.T) {
	g := New()
	defer g.Close()
	g.LoadUsers("node-1", []User{mkUser(1, 1, 0)})
	ctx := context.Background()
	if err := g.Admit(ctx, "node-1", emailOf(1), "10.0.0.1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := g.Admit(ctx, "node-1", emailOf(1), "10.0.0.2"); err != ErrDeviceLimit {
		t.Fatalf("want limit: %v", err)
	}
	g.UpdateUser("node-1", nil, nil, []User{mkUser(1, 5, 0)})
	if err := g.Admit(ctx, "node-1", emailOf(1), "10.0.0.2"); err != nil {
		t.Fatalf("after raising limit: %v", err)
	}
}

// 在线设备快照用于 alive 上报。
func TestOnlineSnapshot(t *testing.T) {
	g := New()
	defer g.Close()
	g.LoadUsers("node-1", []User{mkUser(1, 0, 0)})
	if err := g.Admit(context.Background(), "node-1", emailOf(1), "10.0.0.1"); err != nil {
		t.Fatalf("admit: %v", err)
	}
	online := g.Online("node-1")
	if len(online) != 1 || online[0].UID != 1 || online[0].IP != "10.0.0.1" {
		t.Fatalf("unexpected snapshot: %+v", online)
	}
}
