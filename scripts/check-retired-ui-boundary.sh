#!/usr/bin/env sh
# Boundary guard: the retired controller-shell Orchestrator UI must never return.
#
# The Orchestrator UI source is frontend/ (React/Vite/TypeScript, single
# product) and it is built in CI. The shipped bundle is still the pinned
# packaging/orchestrator-ui-baseline/ and is not rebuilt by CI. The obsolete
# controller-shell (static frontend/orchestrator + concatenated ~185 KB app.js)
# was removed because it was deployed over the current UI by mistake.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"

fail() {
  echo "$*" >&2
  exit 1
}

# 1. The retired source tree, bundle, builder and build-profile helper stay gone.
for gone in \
  frontend/orchestrator \
  packaging/orchestrator-frontend \
  scripts/build-controller-shell.sh \
  scripts/lib/product-build-profile.sh; do
  [ ! -e "$gone" ] || fail "retired controller-shell UI must not live in ly-orchestrator: $gone"
done

# 2. No controller-shell source file may reappear. These module and stylesheet
#    names only exist in the retired static shell.
found=$(find . -path ./.git -prune -o -path ./node_modules -prune -o -type f \
  \( -name nic-settings.js -o -name group-settings.js \
     -o -name forms.css -o -name responsive.css \) -print)
[ -z "$found" ] || fail "retired controller-shell UI source found:
$found"

# 3. A built controller-shell bundle is a small concatenated app.js (~185 KB)
#    carrying the product entrypoint marker. The React bundle is ~985 KB.
found=$(find . -path ./.git -prune -o -path ./node_modules -prune -o \
  -type f -name app.js -size -400k -print)
for file in $found; do
  if grep -q 'LY_ROUTE_PRODUCT_ENTRYPOINT' "$file" 2>/dev/null; then
    fail "controller-shell bundle found: $file (build the React UI instead)"
  fi
done

# 4. The React/Vite source tree lives in frontend/ and must stay buildable.
for required in frontend/package.json frontend/index.html frontend/vite.config.ts frontend/src/main.tsx; do
  [ -f "$required" ] || fail "orchestrator UI source is incomplete: $required"
done

# 5. The pinned React baseline is the only UI artifact this repository ships.
#    If it is ever moved or replaced, update this guard together with the move.
baseline="packaging/orchestrator-ui-baseline"
product_build_require() { [ -f "$1" ] || fail "$2"; }
product_build_require "$baseline/app.js" "pinned orchestrator React UI baseline is missing: $baseline/app.js"
product_build_require "$baseline/index.html" "pinned orchestrator React UI baseline is missing: $baseline/index.html"
size=$(wc -c <"$baseline/app.js" | tr -d ' ')
[ "$size" -ge 900000 ] || \
  fail "pinned orchestrator UI app.js is ${size} bytes; a ~185 KB controller-shell build must never replace the React bundle"
grep -q 'id="root"' "$baseline/index.html" || \
  fail "pinned orchestrator UI is not a React bundle (index.html lacks the #root mount point)"

# 6. No script may reference the retired builder or bundle again. Documentation
#    may name the removed paths so the decision stays discoverable.
if grep -R -n --exclude-dir=.git --exclude-dir=docs --exclude=README.md \
  --exclude=check-retired-ui-boundary.sh \
  -e 'build-controller-shell' \
  -e 'orchestrator-frontend' \
  -e 'frontend/orchestrator' . ; then
  fail "reference to the retired controller-shell UI must be removed (see above)"
fi

printf 'retired controller-shell UI boundary holds\n'
