# GeoCN

> 识别来源ip是否为中国ip

## install

```bash
xcaddy build --with github.com/ysicing/caddy2-geocn
```

## usage

```caddyfile
    @geofilter {
        geocn {
          geolocal "./Country.mmdb"
        }
    }
    file_server @geofilter {
        # TODO
    }
    file_server {
        # TODO
    }
```
