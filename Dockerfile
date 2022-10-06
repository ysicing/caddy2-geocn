FROM ysicing/god as builder

RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

WORKDIR /go/src/github.com/ysicing/caddy2-geocn

COPY . .

RUN xcaddy build --with github.com/ysicing/caddy2-geocn=../caddy2-geocn

FROM ysicing/debian as geoip

WORKDIR /root

RUN wget https://github.com/Hackl0us/GeoIP2-CN/raw/release/Country.mmdb

FROM ysicing/debian

COPY docker /etc/caddy

COPY --from=builder /go/src/github.com/ysicing/caddy2-geocn/caddy /usr/local/bin/caddy

COPY --from=geoip /root/Country.mmdb /usr/local/bin/caddy

RUN chmod +x /usr/local/bin/caddy

CMD caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
