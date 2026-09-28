package main

import (
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/debug"
)

// versionInfo は、GET /version が返す JSON の形。goronation serve と goronation web は別プロセス・別ライフサイクル
// (片方だけ再起動できる) なので、将来のバージョン食い違いに備えて、まず「取れる情報を返すだけ」の
// エンドポイントを用意しておく (不一致の検知・警告は、次回以降の課題)。新しい依存は増やさず、
// runtime/debug.ReadBuildInfo (Go 1.18+) が、git リポジトリ内でのビルドなら自動で埋める VCS 情報を使う。
type versionInfo struct {
	GoVersion string `json:"goVersion"`
	Revision  string `json:"revision,omitempty"`
	Time      string `json:"time,omitempty"`
	Modified  bool   `json:"modified,omitempty"`
}

// buildVersionInfo は、実行中のバイナリの versionInfo を組み立てる。go run 等、VCS 情報が埋まらない
// ビルドでは、Revision/Time は空のままになる (エラーにはしない)。
func buildVersionInfo() versionInfo {
	v := versionInfo{GoVersion: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				v.Revision = s.Value
			case "vcs.time":
				v.Time = s.Value
			case "vcs.modified":
				v.Modified = s.Value == "true"
			}
		}
	}
	return v
}

// handleVersion は、GET /version: 認証を問わず返す (goronation serve の UDS では、繋げること自体が信頼の
// 境界であり、バージョン情報自体も秘密ではない)。
func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildVersionInfo())
}
