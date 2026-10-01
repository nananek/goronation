package v0

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// form の語彙 (ADR 0040): エージェントが、人間に、フィールドの一覧を入力させる要求。claude の AskUserQuestion と、opencode の form
// (question の tool・web 検索の provider の選択など。metadata.kind で種類が分かれる) を、同じ形で表す。
// 「質問」でなく「form」として定める: 同じ仕組みが、質問以外の入力 (設定の選択) にも使われる (採取: opencode の websearch-form)。

// フィールドの type。
const (
	// FieldSelect は、options から 1 つを選ぶ (Custom が true なら、options に無い文字列も入力できる)。
	FieldSelect = "select"
	// FieldMultiselect は、options から 0 個以上を選ぶ (Custom が true なら、options に無い文字列も足せる)。
	FieldMultiselect = "multiselect"
	// FieldText は、自由な文字列を 1 つ入力する (options は無い)。
	FieldText = "text"
)

// form の kind。エージェントが決める種類で、UI は、知らない kind も、フィールドの一覧として表示する (kind で挙動を変えない)。
const (
	// FormKindQuestion は、エージェントが利用者に聞く質問 (claude の AskUserQuestion・opencode の question の tool)。
	FormKindQuestion = "question"
)

// form の決着の outcome。
const (
	FormAnswered  = "answered"
	FormCancelled = "cancelled"
)

// 上限。超える要求は、アダプタが agent.frame にする (承認できない形にして落とす。lenient)。長さは文字数 (rune)。
const (
	MaxFormFields       = 16
	MaxFormOptions      = 32
	MaxFormKeyLen       = 64
	MaxFormKindLen      = 64
	MaxFormTitleLen     = 200
	MaxFormLabelLen     = 200
	MaxFormValueLen     = 200
	MaxFormDescription  = 1000
	MaxFormAnswerText   = 4000
	MaxFormAnswerValues = MaxFormOptions + 8 // multiselect の回答の個数 (options + 自由記述の追加)
)

// FormOption は、select・multiselect の選択肢。Value は、回答で返す値 (無ければ Label と同じ)。
type FormOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// FormField は、form の 1 つのフィールド。Key は、回答のキー (form の中で一意)。
// Title・Description・Label は、エージェントが書いた文で、検証されていない (UI は、エージェントの自己申告として表示する。ADR 0015)。
type FormField struct {
	Key         string       `json:"key"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Type        string       `json:"type"`
	Options     []FormOption `json:"options,omitempty"`
	// Custom は、options に無い文字列も入力できること。
	Custom   bool `json:"custom,omitempty"`
	Required bool `json:"required,omitempty"`
}

// FormRequested は、TypeFormRequested の data。
type FormRequested struct {
	// RequestID は、応答 (CommandFormResolve) に使う、エージェントが振った ID。
	RequestID string `json:"request_id"`
	// CallID は、対応する tool 呼び出し (あれば)。
	CallID string `json:"call_id,omitempty"`
	// Kind は、種類 (FormKindQuestion か、エージェント固有の文字列。例: opencode の "websearch.provider")。
	Kind   string      `json:"kind"`
	Title  string      `json:"title,omitempty"`
	Fields []FormField `json:"fields"`
	// ContentHash は、この要求の内容の SHA-256 (Hash の値)。goronation の共通の層が付ける。アダプタは付けなくてよい (付けても上書きされる)。
	ContentHash string `json:"content_hash,omitempty"`
}

// FormValue は、1 つのフィールドへの回答。select・text は 1 つの文字列、multiselect は文字列の配列 (JSON 上の形)。
type FormValue struct {
	Multi  bool
	Values []string
}

// Text は、1 つの文字列の回答 (select・text) を作る。
func Text(s string) FormValue { return FormValue{Values: []string{s}} }

// Many は、文字列の配列の回答 (multiselect) を作る。
func Many(v ...string) FormValue { return FormValue{Multi: true, Values: v} }

// MarshalJSON は、回答を JSON にする: select・text は文字列 1 つ、multiselect は文字列の配列 (空でも配列)。単一の回答の値が 1 つでなければ error。
func (v FormValue) MarshalJSON() ([]byte, error) {
	if v.Multi {
		if v.Values == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(v.Values)
	}
	if len(v.Values) != 1 {
		return nil, errors.New("v0: 単一の回答は、文字列がちょうど 1 つ")
	}
	return json.Marshal(v.Values[0])
}

// UnmarshalJSON は、JSON の文字列を単一の回答に、文字列の配列を複数の回答にする。それ以外 (数・真偽・null・オブジェクト・配列の中の非文字列) は error。
func (v *FormValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return errors.New("v0: 空の回答")
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = Text(s)
	case '[':
		var a []string
		if err := json.Unmarshal(b, &a); err != nil {
			return err
		}
		*v = FormValue{Multi: true, Values: a}
	default:
		return errors.New("v0: 回答は、文字列か、文字列の配列")
	}
	return nil
}

// FormResolve は、CommandFormResolve の data。Outcome が FormAnswered なら Answer が要り、FormCancelled なら Answer は無い。
type FormResolve struct {
	RequestID string               `json:"request_id"`
	Outcome   string               `json:"outcome"`
	Answer    map[string]FormValue `json:"answer,omitempty"`
	// ContentHash は、利用者が見た要求の内容の SHA-256 (FormRequested.ContentHash の写し)。要求の値と違えば、拒否する (ADR 0042)。
	ContentHash string `json:"content_hash,omitempty"`
}

// ErrInvalidForm は、形の不正 (上限・型・重複・空)。
var ErrInvalidForm = errors.New("v0: invalid form")

func invalidForm(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidForm}, a...)...)
}

// Validate は、要求の形を検査する: request_id・kind が空でなく、フィールドの数・key の一意・type・options の数と、文字数の上限。
// 検査に通らない要求は、人間に見せない・応答できない形なので、アダプタは agent.frame にする (承認できない側に倒す)。
func (f FormRequested) Validate() error {
	if f.RequestID == "" {
		return invalidForm("request_id が空")
	}
	if n := utf8.RuneCountInString(f.Kind); n == 0 || n > MaxFormKindLen {
		return invalidForm("kind の長さ %d", n)
	}
	if utf8.RuneCountInString(f.Title) > MaxFormTitleLen {
		return invalidForm("title が長い")
	}
	if n := len(f.Fields); n == 0 || n > MaxFormFields {
		return invalidForm("フィールドの数 %d (1〜%d)", n, MaxFormFields)
	}
	seen := map[string]bool{}
	for i, fd := range f.Fields {
		if n := utf8.RuneCountInString(fd.Key); n == 0 || n > MaxFormKeyLen {
			return invalidForm("fields[%d].key の長さ %d", i, n)
		}
		if seen[fd.Key] {
			return invalidForm("fields[%d].key %q が重複", i, fd.Key)
		}
		seen[fd.Key] = true
		if utf8.RuneCountInString(fd.Title) > MaxFormTitleLen || utf8.RuneCountInString(fd.Description) > MaxFormDescription {
			return invalidForm("fields[%d] の title・description が長い", i)
		}
		switch fd.Type {
		case FieldSelect, FieldMultiselect:
			if n := len(fd.Options); n == 0 || n > MaxFormOptions {
				return invalidForm("fields[%d].options の数 %d (1〜%d)", i, n, MaxFormOptions)
			}
			vals := map[string]bool{}
			for j, o := range fd.Options {
				if o.Value == "" || utf8.RuneCountInString(o.Value) > MaxFormValueLen ||
					o.Label == "" || utf8.RuneCountInString(o.Label) > MaxFormLabelLen ||
					utf8.RuneCountInString(o.Description) > MaxFormDescription {
					return invalidForm("fields[%d].options[%d] の label・value・description が空か長い", i, j)
				}
				if vals[o.Value] {
					return invalidForm("fields[%d].options[%d].value %q が重複", i, j, o.Value)
				}
				vals[o.Value] = true
			}
		case FieldText:
			if len(fd.Options) != 0 {
				return invalidForm("fields[%d]: text に options は無い", i)
			}
		default:
			return invalidForm("fields[%d].type %q", i, fd.Type)
		}
	}
	return nil
}

// Validate は、回答を、要求 req に対して検査する。Outcome が FormCancelled なら、Answer は無い。FormAnswered なら:
// 全てのキーが req のフィールドの key で、型が合い (select・text は 1 つ、multiselect は配列)、options だけのフィールド (Custom が false)
// は、値が options の Value のどれかで、Required のフィールドは空でなく、文字数が上限内であること。
// 回答の値は、クライアントが送るので、敵対入力として扱う: 要求の内容 (保持した req) に対してだけ検査し、通らなければ何も返さない。
func (r FormResolve) Validate(req FormRequested) error {
	if r.RequestID != req.RequestID {
		return invalidForm("request_id が一致しない")
	}
	switch r.Outcome {
	case FormCancelled:
		if len(r.Answer) != 0 {
			return invalidForm("cancelled に answer がある")
		}
		return nil
	case FormAnswered:
	default:
		return invalidForm("outcome %q", r.Outcome)
	}
	fields := map[string]FormField{}
	for _, fd := range req.Fields {
		fields[fd.Key] = fd
	}
	for k, v := range r.Answer {
		fd, ok := fields[k]
		if !ok {
			return invalidForm("answer のキー %q が、フィールドに無い", k)
		}
		if err := checkValue(fd, v); err != nil {
			return fmt.Errorf("%w (キー %q)", err, k)
		}
	}
	for _, fd := range req.Fields {
		if fd.Required {
			v, ok := r.Answer[fd.Key]
			if !ok || len(v.Values) == 0 || (!v.Multi && v.Values[0] == "") {
				return invalidForm("必須のフィールド %q が空", fd.Key)
			}
		}
	}
	return nil
}

func checkValue(fd FormField, v FormValue) error {
	switch fd.Type {
	case FieldSelect, FieldText:
		if v.Multi || len(v.Values) != 1 {
			return invalidForm("単一の値が要る")
		}
	case FieldMultiselect:
		if !v.Multi {
			return invalidForm("配列が要る")
		}
		if len(v.Values) > MaxFormAnswerValues {
			return invalidForm("値が多すぎる")
		}
	}
	options := map[string]bool{}
	for _, o := range fd.Options {
		options[o.Value] = true
	}
	for _, s := range v.Values {
		if utf8.RuneCountInString(s) > MaxFormAnswerText {
			return invalidForm("値が長い")
		}
		if fd.Type != FieldText && !fd.Custom && !options[s] {
			return invalidForm("選択肢に無い値 %q (custom でない)", s)
		}
	}
	if fd.Type == FieldMultiselect {
		sorted := slices.Clone(v.Values)
		slices.Sort(sorted)
		if len(slices.Compact(sorted)) != len(sorted) {
			return invalidForm("値が重複している")
		}
	}
	return nil
}
