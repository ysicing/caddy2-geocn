## GeoCN

> 识别来源ip是否为中国ip

## install

```bash
xcaddy build --with github.com/ysicing/caddy2-geocn
```

## usage

```caddyfile
    @geofilter {
        geocn {
            db_file "./Country.mmdb"
        }
    }
    redir @geofilter https://www.baidu.com${url} permanent
```