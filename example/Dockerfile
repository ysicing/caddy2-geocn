FROM ghcr.io/ysicing/debian

RUN mkdir -p /etc/caddy

COPY caddy /usr/local/bin/caddy

COPY Country.mmdb /etc/caddy/Country.mmdb

COPY example/Caddyfile /etc/caddy/Caddyfile

COPY example/allow /etc/caddy/example/allow

COPY example/deny /etc/caddy/example/deny

CMD caddy run --config /etc/caddy/Caddyfile --adapter caddyfile