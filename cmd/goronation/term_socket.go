//go:build linux

package main

import "path/filepath"

// defaultGroup は、goronation-web-plan §2 の決定どおりの、まだグループという概念が無いことの固定の
// プレースホルダ。将来グループを作るときは、この定数への依存を、実際のグループ ID を受け取る形
// (--group フラグなど) に置き換える。termSocketPath 自身の形は、そのときも変えなくて済む。
const defaultGroup = "default"

// termSocketPath は、group・sessionID の端末ビュー用 UDS の path。goronation serve (bind する側) と
// goronation web (dial する側) の、どちらも同じ関数で計算するので、値を受け渡す必要が無い (goronation-web-plan
// §2 の決定: 将来グループという 1 段上の単位を挟めるよう、フラットな <state>/sessions/<id>/term.sock
// ではなく、<state>/groups/<group>/sessions/<id>/term.sock という階層にする)。
func termSocketPath(stateDir, group, sessionID string) string {
	return filepath.Join(stateDir, "groups", group, "sessions", sessionID, "term.sock")
}
