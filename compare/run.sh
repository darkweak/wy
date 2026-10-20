#!/bin/bash
# Runs each program with `go run` (the reference), wy (compiled), wy -tree
# (tree-walking interpreter) and yaegi, reporting whether each matches the
# reference output, with timings. Requires wy and yaegi on PATH
# (go install github.com/traefik/yaegi/cmd/yaegi@latest).
cd "$(dirname "$0")"
t() { local s=$(date +%s%N); out=$("$@" 2>&1); el=$(( ($(date +%s%N) - s) / 1000000 )); }
printf "%-22s %-16s %-16s %-16s\n" program wy wy-tree yaegi
for f in [0-9]*.go; do
  want=$(go run "$f" 2>&1)
  wy "$f" >/dev/null 2>&1   # first run compiles and caches
  t wy "$f";        g=$([ "$out" == "$want" ] && echo OK || echo DIFF); gt=$el
  t wy -tree "$f";  r=$([ "$out" == "$want" ] && echo OK || echo DIFF); rt=$el
  t yaegi run "$f";  y=$([ "$out" == "$want" ] && echo OK || echo DIFF); yt=$el
  printf "%-22s %-4s %7dms    %-4s %7dms    %-4s %7dms\n" "${f%.go}" "$g" "$gt" "$r" "$rt" "$y" "$yt"
done
