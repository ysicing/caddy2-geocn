package geocn

import (
	"net"
	"strings"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

// detectIPVersion 检测 IP 版本并返回对应的 xdb 版本常量
func detectIPVersion(ip net.IP) *xdb.Version {
	if ip == nil {
		return xdb.IPv4 // 默认 IPv4
	}

	// To4() 返回 nil 说明是 IPv6
	if ip.To4() == nil {
		return xdb.IPv6
	}
	return xdb.IPv4
}

// detectIPVersionFromString 从字符串检测 IP 版本
func detectIPVersionFromString(ipStr string) *xdb.Version {
	// 移除可能的端口号
	if idx := strings.LastIndex(ipStr, ":"); idx > 0 {
		// 检查是否是 IPv6（包含多个冒号）
		if strings.Count(ipStr, ":") > 1 {
			return xdb.IPv6
		}
		// IPv4:port 格式
		ipStr = ipStr[:idx]
	}

	ip := net.ParseIP(strings.TrimSpace(ipStr))
	return detectIPVersion(ip)
}

// getXDBVersion 根据数据库文件名推测支持的 IP 版本
// 如果文件名包含 "ipv6" 则认为是 IPv6 数据库，否则默认 IPv4
func getXDBVersion(dbPath string) *xdb.Version {
	lowerPath := strings.ToLower(dbPath)
	if strings.Contains(lowerPath, "ipv6") {
		return xdb.IPv6
	}
	// 默认假设是 IPv4 数据库
	return xdb.IPv4
}
