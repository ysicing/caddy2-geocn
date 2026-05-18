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
- 🧠 IP 获取：优先使用 Caddy 的 `ClientIPVarKey`（需配置 `trusted_proxies`），回退到 `RemoteAddr`
- 🔄 自动更新 GeoIP2 数据库（默认每 24h 检查）
- 🗄️ 查询结果缓存（默认启用：TTL 5m，容量 10000）
- 🚀 全局单例：所有站点共享同一数据库和缓存，资源高效

### GeoCity 模块
- 🏙️ 支持省份和城市级别的访问控制
- 🧠 IP 获取：优先使用 Caddy 的 `ClientIPVarKey`（需配置 `trusted_proxies`），回退到 `RemoteAddr`
- 🔄 自动更新 ip2region 数据库（默认每 24h 检查）
- 🗄️ 查询结果缓存（默认启用：TTL 5m，容量 10000；可在 Caddyfile 用 `cache off` 关闭）
- 🎯 精确的地理位置识别

## 使用示例

### GeoCN - 中国 IP 识别

GeoCN 采用全局单例模式，配置在全局选项块中，所有站点共享。

#### 基础用法（使用默认配置）

```caddyfile
{
    # 如果在反向代理后面，需要配置 trusted_proxies
    servers {
        trusted_proxies static 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
    }
}

# 只允许中国 IP 访问
china.example.com {
    @china_ip geocn

    handle @china_ip {
        file_server
    }

    handle {
        respond "仅限中国大陆访问" 403
    }
}
```

#### 自定义全局配置

```caddyfile
{
    # GeoCN 全局配置（所有站点共享）
    geocn {
        interval 24h          # 数据库更新检查间隔
        timeout 30s           # 下载超时
        cache ttl 10m size 20000  # 缓存配置
        # cache off           # 关闭缓存
        # source https://example.com/Country.mmdb  # 自定义数据源
    }

    servers {
        trusted_proxies static 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
    }
}

# 站点 1
site1.example.com {
    @cn geocn
    handle @cn {
        respond "Welcome from China!"
    }
    handle {
        respond "Access denied" 403
    }
}

# 站点 2（共享同一 GeoCN 实例）
site2.example.com {
    @cn geocn
    handle @cn {
        reverse_proxy backend:8080
    }
    handle {
        respond "仅限中国访问" 403
    }
}
```

### 缓存与更新

- 默认缓存
  - 开启：默认启用
  - TTL：5m（`cache ttl 5m` 可调整）
  - 容量：10000（`cache size 10000` 可调整）
  - 关闭：Caddyfile 中使用 `cache off`，或 JSON 使用 `enable_cache: false`

- 更新策略
  - 默认每 24 小时检查更新（`interval 24h` 可调整）
  - 远端 HEAD 返回 Last-Modified 时：与本地文件 mtime 比较，变新则更新
  - 远端缺少 Last-Modified 时：按 `interval` 与本地 mtime 判断是否需要刷新

### GeoCity - 省市地区控制

```caddyfile
{
    # 如果在反向代理后面，需要配置 trusted_proxies
    servers {
        trusted_proxies static 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
    }
}

# 只允许北京和上海访问
city.example.com {
    @allowed {
        geocity {
            regions "北京" "上海"
            # 在整个 region 字符串中搜索
            # 例如 "中国|0|北京|北京市|联通" 会匹配 "北京"
        }
    }

    handle @allowed {
        reverse_proxy backend:8080
    }

    handle {
        respond "仅限北京、上海地区访问" 403
    }
}

# 禁止特定省份访问（使用 not 匹配器）
province.example.com {
    @blocked {
        geocity {
            regions "河北" "山东"
        }
    }

    handle @blocked {
        respond "该地区暂不提供服务" 403
    }

    handle {
        file_server
    }
}
```

## GeoCity 说明

- 双栈：支持 IPv4 与 IPv6，自动按 IP 版本选择对应数据库
- 数据源：支持 HTTP URL 或本地文件（分别配置 v4/v6 源）
- 更新：默认每 24h 检查 HTTP 源是否更新，缺少 Last-Modified 时按 `interval` 回退判断
- 缓存：默认启用（TTL 5m，容量 10000），可 `cache off` 关闭
- IP 获取：优先使用 Caddy 的 `ClientIPVarKey`（需配置 `trusted_proxies`），回退到 `RemoteAddr`

配置项：
- `regions`：地区关键词列表，多个关键词为 OR 关系；用 `+` 连接表示 AND（如 `"河北+联通"` 表示同时包含河北和联通）
- `ipv4_source`：IPv4 数据库源（HTTP URL 或本地文件）
- `ipv6_source`：IPv6 数据库源（HTTP URL 或本地文件）
- `interval`：更新检查间隔（默认 `24h`，仅对 HTTP 源生效）
- `timeout`：下载/检查超时（默认 `30s`）
- `cache`：默认启用；可配置 `cache ttl <duration>`、`cache size <number>`

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

- 允许部分省市访问（OR 关系）：
```caddyfile
geocity {
    regions "广东" "浙江" "北京" "上海"
}
```

- 精确匹配省份+运营商（AND 关系，用 `+` 连接）：
```caddyfile
geocity {
    regions "河北+联通"
}
```

- 混合使用（河北联通 OR 北京）：
```caddyfile
geocity {
    regions "河北+联通" "北京"
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
- 非中国 IP 返回 false（不匹配）
- 本地文件作为数据源时不参与定期更新；HTTP 源才会根据 `interval` 检查更新
- 首次运行会自动下载数据库到 `{caddy_data_dir}/geocity/ipv4.xdb` 与 `{caddy_data_dir}/geocity/ipv6.xdb`

## 反向代理配置

当 Caddy 位于反向代理（如 nginx、Cloudflare）后面时，需要配置 `trusted_proxies` 以正确获取客户端真实 IP：

```caddyfile
{
    servers {
        trusted_proxies static 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16
    }
}

example.com {
    @cn geocn
    handle @cn {
        respond "Welcome from China!"
    }
}
```

Cloudflare 示例：

```caddyfile
{
    servers {
        trusted_proxies cloudflare
    }
}
```

> 注意：如果未配置 `trusted_proxies`，模块会回退到使用 `RemoteAddr`，这在有反向代理时可能获取到代理服务器的 IP 而非客户端真实 IP。
