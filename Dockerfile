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

COPY --from=geoip /root/Country.mmdb /etc/caddy/Country.mmdb

RUN chmod +x /usr/local/bin/caddy && \
  mkdir -p \
  /config/caddy \
  /data/caddy \
  /etc/caddy \
  /usr/share/caddy

ENV XDG_CONFIG_HOME /config

ENV XDG_DATA_HOME /data

EXPOSE 80
EXPOSE 443
EXPOSE 443/udp
EXPOSE 2019

CMD caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
