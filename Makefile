build:
	xcaddy build --with github.com/ysicing/chinaip=../caddy2-bancn

run:
	./caddy run --config ./Caddyfile --adapter caddyfile