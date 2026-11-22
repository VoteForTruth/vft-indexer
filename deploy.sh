#!/usr/bin/env bash
# Deploys the indexer to the server nous: checks, builds a static Linux binary,
# uploads it, atomically replaces /opt/vft-indexer/vft-indexer and restarts the
# service. Settings (/etc/vft-indexer/env) and the database
# (/var/lib/vft-indexer) live on the server and are not touched.
# The one-time server setup is in deploy/README.md.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

SSH_HOST="${SSH_HOST:-nous}"
DOMAIN="${DOMAIN:-votefortruth.org}"

echo "==> checks"
test -z "$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ." >&2; exit 1; }
go vet ./...
go test ./...

echo "==> build linux/amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /tmp/vft-indexer-linux ./cmd/vft-indexer

echo "==> upload and restart"
scp -q /tmp/vft-indexer-linux "$SSH_HOST:/tmp/vft-indexer.new"
ssh "$SSH_HOST" 'set -e
sudo install -o root -g root -m 755 /tmp/vft-indexer.new /opt/vft-indexer/vft-indexer.new
sudo mv /opt/vft-indexer/vft-indexer.new /opt/vft-indexer/vft-indexer
rm -f /tmp/vft-indexer.new
sudo systemctl restart vft-indexer
sleep 3
systemctl is-active vft-indexer'

echo "==> health"
for _ in $(seq 1 20); do
	if curl -fsS "https://$DOMAIN/api/health"; then echo; echo "ok"; exit 0; fi
	sleep 3
done
echo "health check failed: https://$DOMAIN/api/health" >&2
ssh "$SSH_HOST" 'sudo journalctl -u vft-indexer -n 30 --no-pager' >&2
exit 1
