#!/usr/bin/env bash
# Upload one file as a Gitea release asset (lab CI helper).
# Usage: ci-upload-release.sh VERSION FILE
# Env: API (http://host/api/v1), REPO (owner/name), GITEA_TOKEN
set -euo pipefail

version=${1:?version required}
file=${2:?file required}
api=${API:?API required}
repo=${REPO:?REPO required}
token=${GITEA_TOKEN:?GITEA_TOKEN required}

name=$(basename "$file")
tag="v${version#v}"

release_json=$(curl -fsS -H "Authorization: token $token" "$api/repos/$repo/releases/tags/$tag" || true)
release_id=$(printf '%s' "$release_json" | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("id",""))
except Exception:
    print("")')

if [ -z "$release_id" ]; then
  body=$(python3 -c 'import json,sys;print(json.dumps({"tag_name":sys.argv[1],"name":"Ly Route "+sys.argv[1],"body":"Ly Route release artifacts"}))' "$tag")
  release_id=$(curl -fsS -H "Authorization: token $token" -X POST "$api/repos/$repo/releases" \
    -H 'Content-Type: application/json' -d "$body" |
    python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
fi

asset_json=$(curl -fsS -H "Authorization: token $token" "$api/repos/$repo/releases/$release_id/assets")
asset_id=$(printf '%s' "$asset_json" | python3 -c "import json,sys
name=sys.argv[1]
for a in json.load(sys.stdin):
    if a.get('name')==name:
        print(a.get('id','')); break
" "$name")

if [ -n "$asset_id" ]; then
  curl -fsS -H "Authorization: token $token" -X DELETE \
    "$api/repos/$repo/releases/$release_id/assets/$asset_id" >/dev/null
fi

curl -fsS -H "Authorization: token $token" -X POST \
  "$api/repos/$repo/releases/$release_id/assets?name=$name" \
  -F "attachment=@$file" >/dev/null
echo "uploaded $name ($(stat -c%s "$file") bytes) to $tag"
