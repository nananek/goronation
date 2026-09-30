// Package framenorm は、採取したフレーム (1 行 1 JSON) の、実行ごとに変わる値 (ID・時刻・所要時間・ポートなど) を、固定の記号に置き換える正規化を持つ (cmd/framecapture と、opencode の serve の採取 (cmd/goronation の test) が共有する)。
//
// 保証する: 同じ場面を採り直した 2 回の出力が、構造的に同じになる (ID は最初に現れた順の番号の記号 "<kind:N>" で、同じ ID は同じ記号)。正規化は冪等で、値の型・キーの有無・行の順序を変えない (JSON のキーの順だけ、キー名の辞書順になる)。
// 規則は、既定 (Rules のゼロ値。claude・opencode run の golden) と、Serve (opencode 2.x の serve の golden) の 2 つ。保証しない: 規則に無い形の秘密・ホストの path の除去 (採取の側で、別に検査する)。標準ライブラリだけを使う。
package framenorm
