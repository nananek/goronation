package credential

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
)

// redacted は、Secret が、どの経路でも出す文字列。
const redacted = "[redacted]"

// ErrNotFound は、名前の資格情報が無いときの error (errors.Is で判定する)。
var ErrNotFound = errors.New("credential: not found")

// ErrInvalidName は、名前が nameRE の形でないときの error (errors.Is で判定する)。
var ErrInvalidName = errors.New("credential: invalid name")

// nameRE は、資格情報の名前の形。path の 1 要素・URL の一部にしても安全な、小文字・数字・- だけ。
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CheckName は、name が資格情報の名前の形か確かめる。形が違えば ErrInvalidName を包んだ error (名前は、値でなく、呼び手が決める文字列)。
// Source の実装は、名前を path や URL に使う前に、これを呼ぶ。
func CheckName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

// Source は、資格情報の取り出し口。名前で引く。
type Source interface {
	// Token は、name の資格情報を返す。無ければ ErrNotFound を、名前の形が違えば ErrInvalidName を、包んだ error を返す。
	// 返す error の文言に、値を含めない。
	Token(ctx context.Context, name string) (Secret, error)
}

// Secret は、資格情報の値。String・GoString・Format・MarshalJSON・MarshalText・LogValue は、全て "[redacted]" で、
// 値を返すのは Reveal だけ。ゼロ値は、空の値。
//
// 値は、フィールドの先の別の変数に置く: Secret を、unexported のフィールドとして持つ構造体を fmt (%v・%+v・%#v) が
// 出力するとき、fmt は、メソッドを呼べず、フィールドを反射で出す。値そのものがフィールドだと、そのまま出てしまう (先のアドレスが出る)。
type Secret struct {
	v *string
}

// New は、値 v の Secret を作る。
func New(v string) Secret { return Secret{v: &v} }

// Reveal は、値を返す。呼び手は、それをログ・error・出力に書かない責任を負う。
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// IsZero は、値が空か (ゼロ値か、空の値で作ったか)。
func (s Secret) IsZero() bool { return s.Reveal() == "" }

// String は、"[redacted]" を返す。
func (Secret) String() string { return redacted }

// GoString は、%#v の出力で、"[redacted]" を返す。
func (Secret) GoString() string { return redacted }

// Format は、fmt の全ての動詞 (%v・%+v・%s・%q・%x・%d など) で、"[redacted]" を出す。
func (Secret) Format(f fmt.State, _ rune) { fmt.Fprint(f, redacted) }

// MarshalJSON は、JSON の文字列 "[redacted]" を返す。
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText は、"[redacted]" を返す (encoding.TextMarshaler。YAML・TOML・map のキーなど)。
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue は、slog の値として、"[redacted]" を返す (slog.LogValuer)。
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }
