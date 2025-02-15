FROM ysicing/god as builder

RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

RUN go install github.com/go-task/task/v3/cmd/task@latest

WORKDIR /go/src/github.com/ysicing/caddy2-geocn

COPY . .

RUN task build

FROM ysicing/debian

COPY docker /etc/caddy

COPY --from=builder /go/src/github.com/ysicing/caddy2-geocn/caddy /usr/local/bin/caddy

COPY --from=builder /go/src/github.com/ysicing/caddy2-geocn/Country.mmdb /etc/caddy/Country.mmdb

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
