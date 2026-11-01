package xray

// xray 的协议实现、传输层、app 都是靠包 init 注册到 core 的：
// 只 import 数据类型（vless.Account 之类）不会注册 inbound/outbound 处理器。
// 缺了这一步，core.New 会报 `common: *proxyman.InboundConfig is not registered`。
// distro/all 是上游自己的全量注册入口，跟着它走最不容易漏。
import (
	_ "github.com/xtls/xray-core/main/distro/all"
)
