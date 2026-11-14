package panel

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// 面板把 node token 放在 query 上（ServerV2 只读 input('token')），
// 任何拼了 URL 的错误都会顺手把令牌写进日志。
func TestScrubRemovesSecrets(t *testing.T) {
	in := `Get "https://panel.example/api/v2/server/config?node_id=1&token=s3cr3t-value": dial tcp: i/o timeout`
	out := scrub(in)
	if strings.Contains(out, "s3cr3t-value") {
		t.Fatalf("token 没被清洗：%s", out)
	}
	if !strings.Contains(out, "token=<redacted>") {
		t.Fatalf("期望 token=<redacted>，实际 %s", out)
	}
	if !strings.Contains(out, "node_id=1") {
		t.Fatalf("不该顺手删掉 node_id：%s", out)
	}
}

func TestScrubRemovesMachineIDAndAPIKey(t *testing.T) {
	out := scrub("machine_id=abc123 api_key=k-999 done")
	if strings.Contains(out, "abc123") || strings.Contains(out, "k-999") {
		t.Fatalf("机器模式凭据泄漏：%s", out)
	}
}

func TestRedactURLKeepsCause(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://p/api?token=abc", Err: errors.New("dial tcp: i/o timeout")}
	out := redact(err).Error()
	if strings.Contains(out, "abc") {
		t.Fatalf("token 泄漏：%s", out)
	}
	if !strings.Contains(out, "<redacted-url>") || !strings.Contains(out, "timeout") {
		t.Fatalf("应保留操作与底层原因：%s", out)
	}
}

func TestRedactPlainErrorIsScrubbed(t *testing.T) {
	out := redact(errors.New("panel 401: /api/v2/server/config?token=zzz")).Error()
	if strings.Contains(out, "zzz") {
		t.Fatalf("非 url.Error 也要清洗：%s", out)
	}
}

func TestRedactNil(t *testing.T) {
	if redact(nil) != nil {
		t.Fatal("nil 应原样返回 nil")
	}
}
