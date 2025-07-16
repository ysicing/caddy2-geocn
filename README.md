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
- 🔄 自动更新 GeoIP2 数据库
- 🚀 高性能 IP 地理位置查询

### GeoCity 模块  
- 🏙️ 支持省份和城市级别的访问控制
- ✅ 白名单和黑名单模式
- 🔄 自动更新 ip2region 数据库
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

## 详细文档

- [GeoCity 详细使用指南](GEOCITY.md) - 省市地区访问控制功能
