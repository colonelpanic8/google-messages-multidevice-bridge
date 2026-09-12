#!/usr/bin/env sh
# Regenerates local-changes.patch against the pinned upstream module cache.
set -eu
cd "$(dirname "$0")"
version=$(awk 'NR==1{print $2}' UPSTREAM)
upstream="$(go env GOMODCACHE)/go.mau.fi/mautrix-gmessages@${version}/pkg/libgm"
[ -d "$upstream" ] || go mod download "go.mau.fi/mautrix-gmessages@${version}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/upstream/pkg" "$work/local/pkg"
cp -R "$upstream" "$work/upstream/pkg/libgm"
cp -R pkg/libgm "$work/local/pkg/libgm"
chmod -R u+w "$work/upstream" "$work/local"
rm -rf "$work/upstream/pkg/libgm/gmtest" "$work/upstream/pkg/libgm/manualdecrypt"
rm -rf "$work/local/pkg/libgm/gmtest" "$work/local/pkg/libgm/manualdecrypt"
rm -f "$work/upstream/pkg/libgm/events/ready_test.go" "$work/upstream/pkg/libgm/patch_test.go"
rm -f "$work/local/pkg/libgm/events/ready_test.go" "$work/local/pkg/libgm/patch_test.go"
status=0
(cd "$work" && git diff --no-index --no-ext-diff --src-prefix=a/ --dst-prefix=b/ -- upstream/pkg/libgm local/pkg/libgm) > local-changes.patch || status=$?
case "$status" in
	0|1) ;;
	*) exit "$status" ;;
esac
changed_lines=$(awk '/^[-+][^-+]/{count++} END{print count+0}' local-changes.patch)
echo "wrote local-changes.patch ($changed_lines changed lines)"
