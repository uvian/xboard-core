package xray

import (
	"fmt"

	"github.com/xboard-ng/xboard-core/internal/panel"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/proxy/vmess"
)

// Email 是 xray 侧的账号标识，也是 usergate 的回调键。
//
// 用 "<uid>.<uuid>" 而不是裸 uuid：面板的 uid 与 uuid 一一对应，但 uuid 可换，
// 拼起来既能反解出 uid（上报流量用），又能在 uuid 变更时自然形成新用户。
func Email(u panel.UserInfo) string { return fmt.Sprintf("%d.%s", u.ID, u.UUID) }

// buildUser 把面板用户翻译成 xray 的 protocol.User。
//
// v26.10.31 的账号类型都退回各自协议包内（proxy/vless/account 子包已不存在），
// 这里按协议挑对应的 proto Account；trojan / shadowsocks 的凭据就是密码本身。
func buildUser(proto string, u panel.UserInfo) (*protocol.User, error) {
	user := &protocol.User{Email: Email(u), Level: 0}
	switch proto {
	case "vless":
		user.Account = serial.ToTypedMessage(&vless.Account{Id: u.UUID})
	case "vmess":
		user.Account = serial.ToTypedMessage(&vmess.Account{Id: u.UUID})
	case "trojan":
		user.Account = serial.ToTypedMessage(&trojan.Account{Password: u.UUID})
	case "shadowsocks":
		// SS 的用户凭据由 inbound 的 server_key 派生，单用户 Account 只带密码。
		user.Account = serial.ToTypedMessage(&shadowsocks.Account{Password: u.UUID})
	default:
		return nil, fmt.Errorf("xray: hot update not implemented for protocol %q", proto)
	}
	return user, nil
}
