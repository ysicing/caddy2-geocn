build:
	xcaddy build --with github.com/ysicing/caddy2-geocn=../caddy2-geocn

run:
	./caddy run --config ./Caddyfile --adapter caddyfile