#!/usr/bin/env bash
# Build a complete, portable Linux release before pushing or publishing anything.
set -euo pipefail
tag=${1:?release tag required}
upstream=${2:?verified upstream ref required}
[[ "$tag" =~ ^pd-v[0-9]+\.[0-9]+\.[0-9]+-[0-9]{14}-[0-9a-f]+$ ]]
root=$(git rev-parse --show-toplevel)
cd "$root"
[[ ! -e dist ]] || { echo 'dist already exists'; exit 1; }
mkdir -p dist
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
commit=$(git rev-parse HEAD)
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X main.Version=$tag -X main.Commit=$commit -X main.BuildDate=$build_date" \
  -o "$stage/cli-proxy-api" ./cmd/server
"$stage/cli-proxy-api" --capabilities > dist/routing-capabilities.json
"$stage/cli-proxy-api" --check-config --config config.example.yaml
cp LICENSE README.md README_CN.md config.example.yaml "$stage/"
cp dist/routing-capabilities.json "$stage/"
upstream_tag=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$upstream")
upstream_sha=$(git rev-parse "$upstream")
python3 - "$tag" "$commit" "$upstream_tag" "$upstream_sha" "$build_date" > dist/release-metadata.json <<'PY'
import json,sys
print(json.dumps(dict(schema_version=1,tag=sys.argv[1],commit=sys.argv[2],upstream_repository="router-for-me/CLIProxyAPI",upstream_tag=sys.argv[3],upstream_commit=sys.argv[4],built_at=sys.argv[5],goos="linux",goarch="amd64",cgo_enabled=False,dynamic_plugins=False),indent=2))
PY
cp dist/release-metadata.json "$stage/"
tar -C "$stage" -czf "dist/CLIProxyAPI_${tag}_linux_amd64.tar.gz" cli-proxy-api LICENSE README.md README_CN.md config.example.yaml routing-capabilities.json release-metadata.json
(cd dist && sha256sum *.tar.gz routing-capabilities.json release-metadata.json > checksums.txt && sha256sum -c checksums.txt)
