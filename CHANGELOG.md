# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2025-09-17]

### Added
- 网络超时控制功能，支持为所有 HTTP 操作设置超时时间
- 高性能 LRU 缓存实现，支持 TTL 过期和大小限制
- IPv4/IPv6 双栈支持，使用 ip2region 库的新版本
- 缓存功能默认启用，使用指针类型区分未设置和显式禁用
- 自动缓存目录管理，使用 Caddy 数据目录存储缓存文件
- 支持 HTTP URL 和本地文件路径作为数据源
- 热更新机制，远程数据源自动定期检查更新
- 全面的单元测试覆盖，包括缓存、更新和并发测试
- IP 版本检测辅助函数 (ipversion.go)
- 配置示例文件 examples/Caddyfile.cache
- Taskfile 任务：自动下载 mmdb 和 xdb 数据库文件

### Changed
- 简化配置结构，移除 LocalFile、LocalDir、IPVersion 等冗余字段
- GeoCN 模块统一使用 Source 字段替代 GeoFile 和 RemoteFile
- GeoCity 模块使用 IPv4Source 和 IPv6Source 分别配置双栈数据源
- 自动生成缓存路径，无需用户手动配置
- 更新所有配置示例以反映新的简化配置方式
- 改进并发安全性，使用 sync.RWMutex 保护共享资源
- 优化数据库更新流程，支持原子替换避免服务中断
- Dockerfile：分离 IPv4 和 IPv6 数据库文件复制
- Taskfile：添加数据库文件清理到 default 任务

### Fixed
- 修复并发访问缓存时的竞态条件
- 解决数据库更新时的内存泄漏问题
- 修复本地文件源的加载逻辑
- 改进错误处理和资源清理

### Documentation
- 更新 README.md 反映新的配置方式
- 添加详细的缓存配置示例和最佳实践
- 完善配置注释说明自动更新机制

## [2024-12-16]

### Changed
- 升级 Caddy 依赖从 v2.10.0 到 v2.10.2
- 升级 GitHub Actions checkout 从 v4 到 v5

## [2024-12-01]

### Added
- GeoCity 模块实现，支持城市级地理位置过滤 (#44)

### Documentation
- 修复文档中的拼写错误

## [2024-11-15]

### Security
- 升级 github.com/cloudflare/circl 修复安全漏洞
- 升级 github.com/go-jose/go-jose/v3 修复安全问题

### Changed
- 升级多个依赖包到最新版本
- 重构构建步骤和 GeoIP2 数据库集成
- 改进地理位置处理逻辑
