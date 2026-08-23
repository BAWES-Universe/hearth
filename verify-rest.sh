#!/usr/bin/env bash
# Hearth REST verification — run on the box against 127.0.0.1:8090
set -u
B=http://127.0.0.1:8090
JAR=/tmp/hearth-cookies.txt
rm -f "$JAR"
echo "== 1. GET /api/health =="
curl -s -w "\nHTTP %{http_code}\n" "$B/api/health"
echo
echo "== 2. GET / (static fallback) =="
curl -s -o /dev/null -w "HTTP %{http_code}, %{size_download} bytes\n" "$B/"
echo
echo "== 3. POST /api/auth/guest (device key -> session uuid + httpOnly cookie) =="
RESP=$(curl -s -c "$JAR" -X POST "$B/api/auth/guest" -H "Content-Type: application/json" -d '{"deviceKey":"verify-device-1","name":"Verify"}')
echo "$RESP"
echo "-- cookie jar --"
grep hearth_session "$JAR" | awk '{print $6" (HttpOnly flag not shown in jar, see -D below)"}'
curl -s -D - -o /dev/null -X POST "$B/api/auth/guest" -H "Content-Type: application/json" -d '{"deviceKey":"verify-device-1"}' | grep -i set-cookie
echo
echo "== 4. GET /api/me with cookie =="
curl -s -b "$JAR" -w "\nHTTP %{http_code}\n" "$B/api/me"
echo
echo "== 5. POST /api/spaces (create) =="
SP=$(curl -s -X POST "$B/api/spaces" -H "Content-Type: application/json" -d '{"name":"Verify Room"}')
echo "$SP"
SPID=$(echo "$SP" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
echo "space id: $SPID"
echo
echo "== 6. GET /api/spaces/{id} (world JSON) =="
curl -s "$B/api/spaces/$SPID" | python3 -m json.tool | head -30
echo
echo "== 7. GET /api/spaces/hearth (default world: 32x32, walls, 2 portals) =="
curl -s "$B/api/spaces/hearth" | python3 -c "
import sys,json
w=json.load(sys.stdin)
tiles=w['tiles']
walls=[t for t in tiles if t['t']=='wall']
print(f'id={w[\"id\"]} size={w[\"width\"]}x{w[\"height\"]} tiles={len(tiles)} walls={len(walls)} portals={len(w[\"portals\"])} spawn={w[\"spawn\"]} zones={len(w[\"zones\"])} entities={len(w[\"entities\"])}')
for p in w['portals']: print('  portal:', p)
"
echo
echo "== 8. GET /api/spaces (list) =="
curl -s "$B/api/spaces" | python3 -c "import sys,json; d=json.load(sys.stdin); print('spaces:', [s['id'] for s in d['spaces']])"
