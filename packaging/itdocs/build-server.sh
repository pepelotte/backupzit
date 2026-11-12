#!/bin/sh
# Builds the console of this fork (with the REST API extensions) as .deb
# and .rpm, together with the official, signed agent packages of the same
# BackupZit version.
#   ./packaging/itdocs/build-server.sh 0.48.2 itdocs.1
# -> dist/backupzit-server_0.48.2+itdocs.1_amd64.deb (and .rpm)
#
# The console reports itself as 0.48.2+itdocs.1. Official agents 0.48.2
# count as up to date; the console's update page would still offer newer
# official releases, which do not contain the extensions until they are
# merged upstream.
set -eu
BASE=${1:?usage: build-server.sh <official version, e.g. 0.48.2> <suffix, e.g. itdocs.1>}
SUFFIX=${2:?usage: build-server.sh <official version> <suffix>}
ARCH=${ARCH:-amd64}
VERSION="$BASE+$SUFFIX"
cd "$(dirname "$0")/../.."
go run ./tools/licenses > THIRD_PARTY_LICENSES.txt
rm -rf bin/linux && mkdir -p bin/linux/agents dist
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o bin/linux/backupzit-server ./cmd/backupzit-server
# The console offers these for download and uses them to update agents.
go run ./tools/officialagents "$BASE" bin/linux/agents
export VERSION ARCH
NFPM="go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.43.0"
for fmt in deb rpm; do
    $NFPM package --config packaging/linux/nfpm-server.yaml --packager "$fmt" --target dist/
done
(cd dist && sha256sum backupzit-server* > SHA256SUMS)
ls -l dist/
