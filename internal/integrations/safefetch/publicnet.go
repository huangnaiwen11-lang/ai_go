// Package safefetch 实现对象存储基线 §7.1 要求的安全出站下载。
//
// 它只做一件事：把「一段外部 URL」变成「一段受控字节」，并在任何一步发现目标
// 可能落在内网时拒绝。规则来源是 docs/对象存储_配置基线.md §7.1，属**冻结口径**
// ——这里只做对齐，不做改进。任何偏离都必须先由用户显式批准并回写基线文档。
//
// 与基线/原实现的三处差异，逐条列出（两处是编码差异，一处是收紧）：
//
//  1. **字节上限是两重而不是三重**。Node 侧靠 axios 的 `maxBodyLength` +
//     `maxContentLength` + 流式计数做三重限制，其中 `maxBodyLength` 约束的是
//     **请求体**。本实现只发出 GET，没有请求体，因此收敛为两重：`Content-Length`
//     声明值预检 + 流式计数。**上限数值与语义不变。**
//
//  2. **`MaxRedirects` 的零值语义**。Node 侧「显式传 0」表示不允许重定向、
//     「不传」表示默认 3，Go 的 `int` 零值无法区分这两者。本包的约定是 0 取默认
//     3、负数表示不允许重定向，上限一律硬 clamp 到 0..5。
//
//  3. **裸 IP 一律拒绝（收紧）**。基线把这件事写成两条：「IP 字面量必须通过公网
//     地址校验」与「provider 结果 URL 形如 `^https?://\d+\.\d+\.\d+\.\d+[:/]` 一律
//     拒绝」。本包在 Cling 中只服务于 provider 结果抓取，没有调用方需要按 IP
//     字面量抓取，因此合并为最严的一条：**不区分是否公网，裸 IP 一律拒绝**，且
//     IPv4 / IPv6 同等对待（基线那条正则只写了 IPv4，但「无法用证书证明这就是
//     对方说的那个主机」对 IPv6 完全一样）。详见 validateTargetURL 的注释。
//
// 另有一处**不是**放宽、但值得记下来的行为选择：响应体压缩。原项目走 axios，
// 默认请求 gzip 并由 axios 透明解压；Go 的 http.Transport 只对 gzip 做同样的事。
// 本包刻意不写 `Accept-Encoding`、也不关 `DisableCompression`，让传输层自己处理，
// 于是字节上限统计的是**解压后**的字节（= 调用方真正拿到的字节）。任何没被解开
// 的编码由 Fetch 显式拒绝（ErrUnsupportedEncoding），而不是把压缩字节当图片存进
// 对象存储。
package safefetch

import "net/netip"

// nonPublicPrefixes 是基线 §7.1 的完整非公网地址段清单：IPv4 15 段 + IPv6 17 段。
//
// 顺序与基线一致，便于逐项对照；判定是「命中即拒绝」，与顺序无关。刻意不从
// 标准库的 IsPrivate/IsLoopback 拼凑：那些辅助方法覆盖不到 100.64/10、
// 192.0.2/24、2001:db8::/32 这类保留段，而基线要求逐段一致。
var nonPublicPrefixes = []netip.Prefix{
	// IPv4（15 段）
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6（17 段）
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// IsPublicAddress 报告一个已解析地址是否可用于出站下载。
//
// IPv4-mapped 地址（`::ffff:a.b.c.d`）一律拒绝：它既能表示公网 IPv4，也能绕过
// 只按前缀判定的检查。基线把 `::ffff:0:0/96` 列为非公网，这里用 Is4In6 显式
// 表达同一条规则，避免依赖前缀的书写形式。
func IsPublicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Is4In6() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
