#!/usr/bin/env bash
set -euo pipefail
if [[ -n "$(gofmt -l .)" ]]; then
  echo 'Go formatting differs; run gofmt -w .'
  exit 1
fi
go test ./...
go build -o test-output ./cmd/server
trap 'rm -f test-output' EXIT
./test-output --capabilities | python3 -c 'import json,sys; c=json.load(sys.stdin); assert c["schema_version"]==1 and c["config_check"] and "soonest-reset-first" in c["routing_strategies"] and c["routing_status_path"]=="/v8/management/routing/status" and c["safe_drain"] is True and c["drain_schema_version"]==1 and c["drain_status_path"]=="/v8/management/server/drain"'
./test-output --check-config --config config.example.yaml
rm test-output
