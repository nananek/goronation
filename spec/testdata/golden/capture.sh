#!/usr/bin/env bash
# golden fixtures を採取する (README は置かない。手順はこのコメントと、cmd/framecapture/doc.go、
# scenarios/*.json の description に書く)。
#
#   capture.sh           scenarios/*.json の全場面を、claude・opencode で採取し直して、この directory に書く
#   capture.sh --check   採取し直して、コミット済みの fixtures と一致するかだけを確かめる (書かない)
#
# fixtures は、cmd/framecapture が、bwrap の檻の中で実物の claude・opencode を fake provider に向けて
# 動かして採る。場面 (scenarios/*.json) の選び方は、各 JSON の description に書く。fixtures は、
# framecapture --normalize で、実行ごとに変わる値 (ID・時刻・所要時間) を固定の記号に置き換えてある
# (規則は cmd/framecapture の normalize.go)。同じ場面を何度採り直しても、--check は一致する。
# 採取したバージョン: claude 2.1.284、opencode 1.18.32 (opencode-linux-x64 の tgz の中の ELF。
# `npm pack opencode-linux-x64@1.18.32` で得る。lifecycle script は実行しない)。エージェントの版が
# 変わると、フレームも変わりうる。その差が、追従すべき変更 (fixture の更新) か、壊れか (直す) かを見る。
#
# 必要な環境変数 (すべて絶対 path):
#   FRAMECAPTURE  cmd/framecapture のビルド結果 (go build -o "$FRAMECAPTURE" ./cmd/framecapture)
#   GORONATION    cmd/goronation のビルド結果 (go build -o "$GORONATION" ./cmd/goronation)
#   CLAUDE_BIN    claude の実行ファイル (symlink でなく実体)
#   OPENCODE_BIN  opencode の実行ファイル (ラッパーでなく実体の ELF。上の npm pack の説明を参照)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check=0
[ "${1:-}" = "--check" ] && check=1
for v in FRAMECAPTURE GORONATION CLAUDE_BIN OPENCODE_BIN; do
	[ -n "${!v:-}" ] || { echo "capture.sh: 環境変数 $v が要る" >&2; exit 2; }
done

dest="$here"
if [ "$check" = 1 ]; then
	dest="$(mktemp -d)"
	trap 'rm -rf "$dest"' EXIT
fi
mkdir -p "$dest/claude" "$dest/opencode"

status=0
for scenario in "$here"/scenarios/*.json; do
	name="$(basename "$scenario" .json)"
	for agent in claude opencode; do
		# claude_only の場面 (権限承認の対話採取など) は、opencode では回さない。
		if [ "$agent" = opencode ] && grep -q '"claude_only": *true' "$scenario"; then
			continue
		fi
		case "$agent" in
			claude) bin="$CLAUDE_BIN"; ext=jsonl ;;
			opencode) bin="$OPENCODE_BIN"; ext=ndjson ;;
		esac
		out="$dest/$agent/$name.$ext"
		"$FRAMECAPTURE" "$agent" --goronation-bin "$GORONATION" --agent-bin "$bin" \
			--scenario "$scenario" --normalize --out "$out" 2>/dev/null \
			|| { echo "capture.sh: $agent/$name の採取に失敗" >&2; status=1; continue; }
		if [ "$check" = 1 ] && ! diff -u "$here/$agent/$name.$ext" "$out" >&2; then
			echo "capture.sh: $agent/$name が、コミット済みの fixture と違う" >&2
			status=1
		fi
	done
done
exit "$status"
