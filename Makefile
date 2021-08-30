build:
	xcaddy build --with github.com/ysicing/caddy2bancn=../caddy2-bancn

run:
	./caddy run --config ./Caddyfile --adapter caddyfile