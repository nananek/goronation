package webauthn

import (
	"fmt"
	"net/url"
)

// Config は、RP (Relying Party。goronation serve 自身) の設定。起動時に一度作り、以後は変えない。
type Config struct {
	// RPID は、WebAuthn の RP ID (Origin のホスト名と、完全に一致すること。サブドメインへの拡張は対応しない)。
	RPID string
	// RPName は、登録画面に出す、人間向けの名前 (省略可)。
	RPName string
	// Origin は、ブラウザから見た、この goronation serve の origin ("https://host[:port]"。末尾のスラッシュ無し)。
	// TLS 終端は goronation serve 自身ではなく、リバースプロキシ (tailscale serve を想定) が行う前提 (goronation-serve-plan
	// §4-1)。https を必須にする (WebAuthn の secure context の要件)。例外は http://localhost・http://127.0.0.1
	// (開発用。仕様が secure context とみなす)。
	Origin string
}

// Validate は、cfg が起動時に使える形かを確かめる (goronation serve の起動時に呼ぶ。通らなければ起動しない)。
func (cfg Config) Validate() error {
	if cfg.RPID == "" {
		return fmt.Errorf("webauthn: RPID が空")
	}
	u, err := url.Parse(cfg.Origin)
	if err != nil {
		return fmt.Errorf("webauthn: Origin %q を解釈できない: %w", cfg.Origin, err)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return fmt.Errorf("webauthn: Origin %q が https でない (http は localhost・127.0.0.1 だけ許す)", cfg.Origin)
	}
	if u.Hostname() != cfg.RPID {
		return fmt.Errorf("webauthn: RPID %q が Origin のホスト名 %q と一致しない", cfg.RPID, u.Hostname())
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("webauthn: Origin %q に path・query・fragment を含めない", cfg.Origin)
	}
	return nil
}
