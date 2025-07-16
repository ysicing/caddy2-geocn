# GeoCity - 基于省市地区的访问控制

GeoCity 是一个基于 ip2region 库的 Caddy 模块，提供精细的地理位置访问控制功能。支持省份和城市级别的白名单/黑名单控制。

## 功能特性

- 🌍 基于 ip2region 数据库的精确地理位置识别
- 🏙️ 支持省份和城市级别的访问控制
- ✅ 支持白名单（allow）和黑名单（deny）两种模式
- 🔄 自动更新 ip2region 数据库
- 🚀 高性能，低内存占用
- 🛡️ 支持多种 IP 获取方式（RemoteAddr, X-Forwarded-For, X-Real-IP）

## 配置参数

### 基本配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `mode` | string | `allow` | 匹配模式：`allow`（白名单）或 `deny`（黑名单） |
| `provinces` | []string | - | 省份列表（根据mode决定是允许还是拒绝） |
| `cities` | []string | - | 城市列表（根据mode决定是允许还是拒绝） |

### 高级配置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `local_file` | string | `{caddy_data_dir}/geocity/ip2region.xdb` | 本地数据库文件路径 |
| `remote_file` | string | `https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region.xdb` | 远程数据库文件地址 |
| `interval` | duration | `24h` | 数据库更新间隔 |
| `timeout` | duration | `30s` | HTTP 请求超时时间 |

## 使用示例

### 1. 基本白名单控制

只允许北京和上海地区访问：

```caddyfile
example.com {
    @beijing_shanghai {
        geocity {
            mode allow
            cities "北京" "上海"
        }
    }
    
    handle @beijing_shanghai {
        file_server
    }
    
    handle {
        respond "仅限北京、上海地区访问" 403
    }
}
```

### 2. 省份级别控制

允许广东、浙江、江苏三省访问：

```caddyfile
example.com {
    @allowed_provinces {
        geocity {
            mode allow
            provinces "广东" "浙江" "江苏"
        }
    }
    
    handle @allowed_provinces {
        reverse_proxy backend:8080
    }
    
    handle {
        respond "地区限制" 403
    }
}
```

### 3. 黑名单模式

拒绝特定地区访问：

```caddyfile
example.com {
    @blocked {
        geocity {
            mode deny
            provinces "河北"
            cities "石家庄"
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

### 4. 混合控制

同时指定省份和城市：

```caddyfile
example.com {
    @geo_control {
        geocity {
            mode allow
            provinces "广东" "浙江"
            cities "北京" "上海" "天津" "重庆"
        }
    }
    
    handle @geo_control {
        reverse_proxy app:3000
    }
    
    handle {
        respond "访问受限" 403
    }
}
```

### 5. 与其他匹配器组合

结合路径匹配器使用：

```caddyfile
api.example.com {
    @api_access {
        path /api/*
        geocity {
            mode allow
            provinces "北京" "上海" "广东"
        }
    }
    
    @public_access {
        path /public/*
    }
    
    handle @api_access {
        reverse_proxy api-server:8080
    }
    
    handle @public_access {
        file_server
    }
    
    handle {
        respond "API 仅限指定地区访问" 403
    }
}
```

## 工作原理

1. **IP 获取**：模块会按以下优先级获取客户端 IP：
   - `RemoteAddr`（直接连接）
   - `X-Forwarded-For` 头部
   - `X-Real-IP` 头部

2. **地理位置查询**：使用 ip2region 数据库查询 IP 对应的地理位置信息

3. **规则匹配**：
   - **白名单模式**：
     - 中国IP：只有在允许列表中的省份/城市才能访问
     - 非中国IP：默认不允许访问
   - **黑名单模式**：
     - 中国IP：拒绝列表中的省份/城市无法访问
     - 非中国IP：默认允许访问

4. **数据库更新**：定期检查并更新 ip2region 数据库文件

## 注意事项

1. **私有 IP**：私有 IP 地址（如 192.168.x.x, 10.x.x.x）会被自动跳过
2. **数据库文件**：首次运行时会自动下载 ip2region 数据库文件
3. **匹配逻辑**：省份和城市名称支持部分匹配（包含关系）
4. **性能考虑**：ip2region 查询性能很高，适合高并发场景
5. **中国 IP**：目前只处理中国境内的 IP 地址

## 构建和安装

1. 添加依赖：
```bash
go get github.com/lionsoul2014/ip2region/binding/golang
```

2. 构建 Caddy：
```bash
xcaddy build --with github.com/ysicing/caddy2-geocn
```

## 故障排除

### 常见问题

1. **数据库下载失败**
   - 检查网络连接
   - 确认 `remote_file` 地址可访问
   - 检查磁盘空间

2. **地理位置识别不准确**
   - ip2region 数据库可能需要更新
   - 某些 IP 段可能没有准确的地理位置信息

3. **匹配不生效**
   - 检查省份/城市名称是否正确
   - 确认客户端 IP 不是私有地址
   - 查看 Caddy 日志获取详细信息

### 调试模式

启用调试日志：

```caddyfile
{
    log {
        level DEBUG
    }
}

example.com {
    @geo {
        geocity {
            mode allow
            cities "北京"
        }
    }
    
    handle @geo {
        respond "OK"
    }
    
    handle {
        respond "Blocked" 403
    }
}
```

## 许可证

本项目采用与原项目相同的许可证。
