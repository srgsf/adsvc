#!/bin/sh
# Starts, stops or lists the federation testbed nodes made by `make fed-data`.
#   scripts/fed.sh up [node]   scripts/fed.sh down [node]   scripts/fed.sh status
# ADSVC (default dist/adsvc) is the binary; FED (default .cache/fed) the testbed directory.
set -eu
ADSVC=${ADSVC:-dist/adsvc}
FED=${FED:-.cache/fed}
NODES="hub spoke mirror stranger naive" # the hub first: the others sync from or to it

cmd=${1:-status}
only=${2:-}

for n in $NODES; do
	[ -z "$only" ] || [ "$only" = "$n" ] || continue
	dir=$FED/nodes/$n
	[ -f "$dir/config.yml" ] || { echo "$n: no $dir/config.yml (make fed-data)" >&2; exit 1; }
	pid=""
	[ -f "$dir/pid" ] && pid=$(cat "$dir/pid") && ! kill -0 "$pid" 2>/dev/null && pid=""
	case $cmd in
	up)
		if [ -n "$pid" ]; then echo "$n: already running ($pid)"; continue; fi
		"$ADSVC" proxy -config "$dir/config.yml" >"$dir/log.txt" 2>&1 &
		echo $! >"$dir/pid"
		echo "$n: started ($!), log $dir/log.txt"
		;;
	down)
		if [ -n "$pid" ]; then kill "$pid"; echo "$n: stopped"; else echo "$n: not running"; fi
		rm -f "$dir/pid"
		;;
	status)
		port=$(sed -n 's/^listen: :\([0-9]*\)/\1/p' "$dir/config.yml")
		state=stopped; [ -z "$pid" ] || state="running ($pid)"
		echo "$n: $state http://localhost:$port"
		;;
	*) echo "usage: fed.sh up|down|status [node]" >&2; exit 2 ;;
	esac
done
