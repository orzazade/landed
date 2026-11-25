#!/bin/bash
# report-times.sh [HOURS]: time a cold and a warm `landed report` on a copy of
# the store; the real store is never written.
# Cold: the copy forgets the model answers of the last HOURS hours (default 15,
# the night before a 09:00 report), so the run asks the model again for them.
# Warm: the same command again right after, everything from the copy's cache.
# LANDED (binary) and LANDED_DB (store) override the defaults.
set -euo pipefail
hours=${1:-15}
landed=${LANDED:-$(cd "$(dirname "$0")/.." && pwd)/bin/landed}
db=${LANDED_DB:-$HOME/.local/share/landed/landed.db}
finished=0
owned=0
trap 'rc=$?; if [ "$owned" = 1 ]; then rm -rf "$tmp"; fi; if [ "$finished" != 1 ]; then echo "report-times: FAILED (exit $rc)" >&2; exit 1; fi' EXIT

tmp=$(mktemp -d)
owned=1
sqlite3 "$db" ".backup '$tmp/landed.db'"
cut=$(date -u -v-"${hours}"H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "-${hours} hours" +%Y-%m-%dT%H:%M:%SZ)
# pick_check is made by the first run of a newer landed; an older store has none yet.
sqlite3 "$tmp/landed.db" "create table if not exists pick_check (key text primary key, records integer not null, quote text not null, why text not null, at text not null)"
forgot=$(sqlite3 "$tmp/landed.db" "select (select count(*) from verdict where at >= '$cut') || ' turn labels, ' || (select count(*) from judgment where at >= '$cut') || ' judgments, ' || (select count(*) from landing where at >= '$cut') || ' landings, ' || (select count(*) from pick_check where at >= '$cut') || ' pick checks'; delete from verdict where at >= '$cut'; delete from judgment where at >= '$cut'; delete from landing where at >= '$cut'; delete from pick_check where at >= '$cut';")
echo "store copy forgets the last ${hours}h: $forgot"

run() { # run NAME: time one report on the copy
	rc=0
	/usr/bin/time -p "$landed" report --db "$tmp/landed.db" > "$tmp/$1.out" 2> "$tmp/$1.err" || rc=$?
	if [ "$rc" != 0 ]; then
		echo "$1 run failed (exit $rc):" >&2
		cat "$tmp/$1.err" >&2
		exit 1
	fi
	secs=$(awk '/^real /{print $2}' "$tmp/$1.err")
	echo "$1: ${secs} s"
}
run cold
run warm
echo "cold report: $(head -1 "$tmp/cold.out")"
if [ "$(head -1 "$tmp/cold.out")" = "$(head -1 "$tmp/warm.out")" ]; then
	echo "warm report: same counts"
else
	echo "warm report: $(head -1 "$tmp/warm.out")"
fi
finished=1
