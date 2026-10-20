#!/bin/bash
# Runs the Go repository's own runnable tests ($GOROOT/test, files marked
# "// run") with an engine and counts those that exit 0 with the expected
# output. Usage: compare/gotest.sh wy|yaegi [parallelism]
engine=${1:-wy}
G=$(go env GOROOT)/test
list=$(mktemp)
for d in . ken typeparam fixedbugs chan interface; do
  for f in $G/$d/*.go; do head -1 "$f" | grep -qx "// run" && echo "$f"; done
done > "$list"
one() {
  f=$2; exp=""
  [ -f "${f%.go}.out" ] && exp=$(cat "${f%.go}.out")
  cd "$(dirname "$f")"
  case $1 in
    wy) out=$(timeout 20 wy -tree "$f" 2>&1) ;;
    yaegi) out=$(timeout 20 yaegi run "$f" 2>&1) ;;
  esac
  code=$?
  if [ $code -eq 0 ] && [ "$out" == "$exp" ]; then echo "PASS $f"; else echo "FAIL $f"; fi
}
export -f one
xargs -P "${2:-4}" -I{} bash -c "one $engine {}" < "$list" | sort > "results-$engine.txt"
echo "$engine: $(grep -c PASS "results-$engine.txt") / $(wc -l < "$list") passed (details in results-$engine.txt)"
rm -f "$list"
