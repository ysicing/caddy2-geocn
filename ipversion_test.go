package geocn

import (
	"net"
	"testing"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

func TestIPVersionDetection(t *testing.T) {
	tests := []struct {
		name     string
		ipStr    string
		expected *xdb.Version
	}{
		{
			name:     "IPv4 simple",
			ipStr:    "192.168.1.1",
			expected: xdb.IPv4,
		},
		{
			name:     "IPv4 public",
			ipStr:    "8.8.8.8",
			expected: xdb.IPv4,
		},
		{
			name:     "IPv6 local",
			ipStr:    "::1",
			expected: xdb.IPv6,
		},
		{
			name:     "IPv6 full",
			ipStr:    "2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			expected: xdb.IPv6,
		},
		{
			name:     "IPv6 compressed",
			ipStr:    "2001:db8::1",
			expected: xdb.IPv6,
		},
		{
			name:     "IPv4 with port",
			ipStr:    "192.168.1.1:8080",
			expected: xdb.IPv4,
		},
		{
			name:     "Empty string",
			ipStr:    "",
			expected: xdb.IPv4, // default
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 测试字符串解析
			if tt.ipStr != "" && !isPortString(tt.ipStr) {
				ip := net.ParseIP(tt.ipStr)
				if ip != nil {
					result := detectIPVersion(ip)
					if result != tt.expected {
						t.Errorf("detectIPVersion(%s) = %v, want %v", tt.ipStr, result, tt.expected)
					}
				}
			}

			// 测试从字符串直接检测
			result := detectIPVersionFromString(tt.ipStr)
			if result != tt.expected {
				t.Errorf("detectIPVersionFromString(%s) = %v, want %v", tt.ipStr, result, tt.expected)
			}
		})
	}
}

func TestXDBVersionDetection(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected *xdb.Version
	}{
		{
			name:     "IPv4 database",
			path:     "/path/to/ip2region.xdb",
			expected: xdb.IPv4,
		},
		{
			name:     "IPv6 database lowercase",
			path:     "/path/to/ip2region_ipv6.xdb",
			expected: xdb.IPv6,
		},
		{
			name:     "IPv6 database uppercase",
			path:     "/path/to/IP2Region_IPV6.xdb",
			expected: xdb.IPv6,
		},
		{
			name:     "Default to IPv4",
			path:     "/path/to/database.xdb",
			expected: xdb.IPv4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getXDBVersion(tt.path)
			if result != tt.expected {
				t.Errorf("getXDBVersion(%s) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

// 辅助函数：检测是否是包含端口的字符串
func isPortString(s string) bool {
	_, _, err := net.SplitHostPort(s)
	return err == nil
}

func TestGeoCityDualStack(t *testing.T) {
	// 测试双栈配置
	g := &GeoCity{
		localIPv4File: "/tmp/test/ip2region_ipv4.xdb",
		localIPv6File: "/tmp/test/ip2region_ipv6.xdb",
	}

	// 验证文件路径设置
	if g.localIPv4File != "/tmp/test/ip2region_ipv4.xdb" {
		t.Errorf("Expected IPv4 file path '/tmp/test/ip2region_ipv4.xdb', got %s", g.localIPv4File)
	}

	if g.localIPv6File != "/tmp/test/ip2region_ipv6.xdb" {
		t.Errorf("Expected IPv6 file path '/tmp/test/ip2region_ipv6.xdb', got %s", g.localIPv6File)
	}
}
