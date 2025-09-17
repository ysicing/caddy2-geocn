# GeoCN

> 基于地理位置的 Caddy 访问控制模块

提供两种地理位置匹配器：
- **geocn**: 识别来源 IP 是否为中国 IP（基于 GeoIP2）
- **geocity**: 精细的省市地区访问控制（基于 ip2region）

## 安装

```bash
xcaddy build --with github.com/ysicing/caddy2-geocn
```

## 功能特性

### GeoCN 模块
- 🇨🇳 识别中国 IP 地址
- 🧠 IP 获取优先级：RemoteAddr → X-Forwarded-For(首个) → X-Real-IP
- 🔄 自动更新 GeoIP2 数据库（默认每 24h 检查）
- 🗄️ 查询结果缓存（默认启用：TTL 5m，容量 10000；可在 Caddyfile 用 `cache off` 关闭）
- 🚀 高性能 IP 地理位置查询

### GeoCity 模块  
- 🏙️ 支持省份和城市级别的访问控制
- ✅ 白名单和黑名单模式
- 🔄 自动更新 ip2region 数据库（默认每 24h 检查）
- 🗄️ 查询结果缓存（默认启用：TTL 5m，容量 10000；可在 Caddyfile 用 `cache off` 关闭）
- 🎯 精确的地理位置识别

## 使用示例

### GeoCN - 中国 IP 识别

```caddyfile
# 只允许中国 IP 访问
china.example.com {
    @china_ip {
        geocn
    }
    
    handle @china_ip {
        file_server
    }
    
    handle {
        respond "仅限中国大陆访问" 403
    }
}
```

### 缓存与更新

- 默认缓存（两模块一致）
  - 开启：默认启用
  - TTL：5m（`cache ttl 5m` 可调整）
  - 容量：10000（`cache size 10000` 可调整）
  - 关闭：Caddyfile 中使用 `cache off`，或 JSON 使用 `enable_cache: false`
  - 配置 `size <= 0` 时按默认容量初始化（等同未显式设置）

- 更新策略（两模块一致）
  - 默认每 24 小时检查更新（`interval 24h` 可调整）
  - 远端 HEAD 返回 Last-Modified 时：与本地文件 mtime 比较，变新则更新
  - 远端缺少 Last-Modified 时：按 `interval` 与本地 mtime 判断是否需要刷新

示例：

```caddyfile
:8080 {
    @cn {
        geocn {
            # 更新间隔与超时
            interval 24h
            timeout 30s

            # 缓存（默认已启用，以下为显式设置）
            cache ttl 10m size 20000
            # 关闭缓存：
            # cache off
        }
    }

    handle @cn {
        respond "Welcome from China!"
    }

    handle {
        respond "Access denied" 403
    }
}
```

### GeoCity - 省市地区控制

```caddyfile
# 只允许北京和上海访问
city.example.com {
    @allowed_cities {
        geocity {
            mode allow
            cities "北京" "上海"
        }
    }
    
    handle @allowed_cities {
        reverse_proxy backend:8080
    }
    
    handle {
        respond "仅限北京、上海地区访问" 403
    }
}

# 拒绝特定省份访问
province.example.com {
    @blocked_provinces {
        geocity {
            mode deny
            provinces "河北" "山东"
        }
    }
    
    handle @blocked_provinces {
        respond "该地区暂不提供服务" 403
    }
    
    handle {
        file_server
    }
}
```

## GeoCity 说明

- 双栈：支持 IPv4 与 IPv6，自动按 IP 版本选择对应数据库
- 模式：白名单 allow、黑名单 deny
- 数据源：支持 HTTP URL 或本地文件（分别配置 v4/v6 源）
- 更新：默认每 24h 检查 HTTP 源是否更新，缺少 Last-Modified 时按 `interval` 回退判断
- 缓存：默认启用（TTL 5m，容量 10000），可 `cache off` 关闭
- IP 获取：RemoteAddr → X-Forwarded-For(首个) → X-Real-IP

配置项（精简）：
- `mode`：`allow`（默认）或 `deny`
- `provinces`：省份列表（按模式决定允许/拒绝），支持包含匹配
- `cities`：城市列表（按模式决定允许/拒绝），支持包含匹配
- `ipv4_source`：IPv4 数据库源（HTTP URL 或本地文件）
- `ipv6_source`：IPv6 数据库源（HTTP URL 或本地文件）
- `interval`：更新检查间隔（默认 `24h`，仅对 HTTP 源生效；远端缺少 Last-Modified 时按该值回退判断）
- `timeout`：下载/检查超时（默认 `30s`）
- `cache`：默认启用；可配置 `cache ttl <duration>`、`cache size <number>`；`size <= 0` 视为默认容量

更多配置示例：

```caddyfile
geocity {
    # 自定义数据源（可选）
    # ipv4_source https://cdn.example.com/ip2region_v4.xdb
    # ipv6_source /opt/geodata/ip2region_v6.xdb

    # 更新（仅 HTTP 源）与超时
    interval 24h
    timeout 30s

    # 缓存（默认启用）
    cache ttl 10m size 20000
    # 关闭缓存：
    # cache off
}
```

常见用法：

- 白名单（仅允许部分省市）：
```caddyfile
geocity {
    mode allow
    provinces "广东" "浙江"
    cities "北京" "上海"
}
```

- 黑名单（拒绝部分省市）：
```caddyfile
geocity {
    mode deny
    provinces "河北"
    cities "石家庄"
}
```

- 双栈混合来源（IPv4 本地、IPv6 远程）：
```caddyfile
geocity {
    ipv4_source /data/ip2region_v4.xdb
    ipv6_source https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region_v6.xdb
    interval 24h
}
```

行为说明：
- 私有/环回/链路本地/未指定/组播地址会被跳过
- 非中国 IP：allow 模式下默认不匹配；deny 模式下默认匹配
- 本地文件作为数据源时不参与定期更新；HTTP 源才会根据 `interval` 检查更新
- 首次运行会自动下载数据库到 `{caddy_data_dir}/geocity/ipv4.xdb` 与 `{caddy_data_dir}/geocity/ipv6.xdb`
