#!/usr/bin/env bash
# 実物の git (push) が送る receive-pack の要求の本文を、偽の上流で採取する。
# 偽の上流 (server.py) は、git http-backend (CGI) を包み、POST の本文を、そのまま保存する。
# 使い方: capture.sh <作業ディレクトリ>  (使い捨ての場所。git・python3・gpg が要る)。出力は <作業ディレクトリ>/captures/。
set -euo pipefail
W=$(realpath -m "$1"); HERE=$(cd "$(dirname "$0")" && pwd)
rm -rf "$W"; mkdir -p "$W/srv/o" "$W/captures" "$W/home" "$W/w"
# gpg-agent の socket の path は短くないと作れない (108 バイト): GNUPGHOME は、短い場所に作る。
GH=$(mktemp -d /tmp/gnk.XXXXXX); chmod 700 "$GH"
PORT=${PORT:-18765}
S=20260927-041500-a1b2c3
gitc() { env -i PATH="$PATH" HOME="$W/home" GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GNUPGHOME="$GH" \
  GIT_AUTHOR_NAME=cap GIT_AUTHOR_EMAIL=cap@example.invalid GIT_COMMITTER_NAME=cap GIT_COMMITTER_EMAIL=cap@example.invalid \
  GIT_AUTHOR_DATE="2026-09-27T00:00:00Z" GIT_COMMITTER_DATE="2026-09-27T00:00:00Z" \
  git -c init.defaultBranch=main "$@"; }
mk() { local n=$1; shift
  gitc init -q --bare "$@" "$W/srv/o/$n.git"
  for kv in receive.advertisePushOptions=true receive.certNonceSeed=seed http.receivepack=true; do
    gitc -C "$W/srv/o/$n.git" config "${kv%%=*}" "${kv#*=}"; done; }
mk r; mk r256 --object-format=sha256
echo none > "$W/TAG"
python3 "$HERE/server.py" "$W/srv" "$W/captures" "$PORT" & SRV=$!
trap 'kill $SRV 2>/dev/null || true; GNUPGHOME=$GH gpgconf --kill gpg-agent 2>/dev/null || true; rm -rf "$GH"' EXIT
sleep 1
tag() { echo "$1" > "$W/TAG"; echo "== $1"; }
U=http://127.0.0.1:$PORT/o
cd "$W/w"
gitc init -q c && cd c && gitc remote add origin $U/r.git
echo one > f && gitc add f && gitc commit -qm one
tag create;      gitc push origin HEAD:refs/heads/goro/$S/one 2>&1 | sed 's/^/   /'
echo two >> f && gitc commit -qam two
tag update;      gitc push origin HEAD:refs/heads/goro/$S/one 2>&1 | sed 's/^/   /'
tag progress;    echo three >> f && gitc commit -qam three && gitc push --progress origin HEAD:refs/heads/goro/$S/one 2>&1 | sed 's/^/   /'
tag multi;       gitc push origin HEAD:refs/heads/goro/$S/a HEAD:refs/heads/goro/$S/b 2>&1 | sed 's/^/   /'
tag atomic;      echo four >> f && gitc commit -qam four && gitc push --atomic origin HEAD:refs/heads/goro/$S/a HEAD:refs/heads/goro/$S/b 2>&1 | sed 's/^/   /'
tag copy;        gitc push origin HEAD:refs/heads/goro/$S/copy 2>&1 | sed 's/^/   /'
tag force;       gitc reset -q --hard HEAD~1 && echo four2 >> f && gitc commit -qam four2 && gitc push --force origin HEAD:refs/heads/goro/$S/a 2>&1 | sed 's/^/   /'
tag delete;      gitc push origin :refs/heads/goro/$S/copy 2>&1 | sed 's/^/   /'
tag tag;         gitc tag -a -m t1 t1 && gitc push origin refs/tags/t1 2>&1 | sed 's/^/   /'
tag lighttag;    gitc tag t2 && gitc push origin refs/tags/t2 2>&1 | sed 's/^/   /'
tag main;        gitc push origin HEAD:refs/heads/main 2>&1 | sed 's/^/   /'
tag pushopt;     gitc push -o ci.skip -o k=v origin HEAD:refs/heads/goro/$S/opt 2>&1 | sed 's/^/   /'
tag unicode;     gitc push origin HEAD:refs/heads/goro/$S/日本語 2>&1 | sed 's/^/   /'
tag nested;      gitc push origin HEAD:refs/heads/goro/$S/feat/x/y 2>&1 | sed 's/^/   /'
tag mixed;       gitc push origin HEAD:refs/heads/goro/$S/ok2 HEAD:refs/heads/other 2>&1 | sed 's/^/   /'
# 署名つき push (gpg の鍵を、使い捨ての GNUPGHOME に作る)
gitc_gpg() { gitc -c user.signingkey=cap "$@"; }
env -i PATH="$PATH" HOME="$W/home" GNUPGHOME="$GH" gpg --batch --pinentry-mode loopback --passphrase '' --quick-gen-key cap@example.invalid default default never >/dev/null 2>&1 || echo "   (gpg の鍵を作れない)"
tag signed;      gitc_gpg push --signed=yes origin HEAD:refs/heads/goro/$S/signed 2>&1 | sed 's/^/   /' || true
# shallow: 浅い clone から push
cd "$W/w" && gitc clone -q --depth 1 file://$W/srv/o/r.git sh 2>&1 | sed 's/^/   /'
cd sh && gitc remote set-url origin $U/r.git && echo shallow >> f && gitc commit -qam shallow
tag shallow;     gitc push origin HEAD:refs/heads/goro/$S/shallow 2>&1 | sed 's/^/   /'
# 大きい push (postBuffer を超える → Transfer-Encoding: chunked)
cd "$W/w/c" && head -c 3000000 /dev/urandom > big.bin && gitc add big.bin && gitc commit -qm big
tag big;         gitc push origin HEAD:refs/heads/goro/$S/big 2>&1 | sed 's/^/   /'
# sha256 のリポジトリ
cd "$W/w" && gitc init -q --object-format=sha256 c256 && cd c256 && gitc remote add origin $U/r256.git
echo s > f && gitc add f && gitc commit -qm s
tag sha256;      gitc push origin HEAD:refs/heads/goro/$S/s 2>&1 | sed 's/^/   /'
# fetch (upload-pack) の POST の見出しも採取する (経路と Content-Type の確認用)
tag fetch;       cd "$W/w" && gitc clone -q $U/r.git fetched 2>&1 | sed 's/^/   /'
echo; ls -l "$W/captures"
