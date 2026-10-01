package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	v0 "github.com/nananek/goronation/spec/v0"
)

// claude の AskUserQuestion は form にする (ADR 0040・0044)。回答は、allow の updatedInput に、保持した input と answers (質問の文 → 回答の文字列。
// 複数選択は ", " で連結) を足して返す。質問の文・選択肢は、要求時に保持し、クライアントからは受けない。

// askUserQuestion は、form にする tool の名前 (requires_user_interaction が true でも、tool 名が別なら、通常の承認)。
const askUserQuestion = "AskUserQuestion"

// formDenyMessage は、取り消し (cancelled) のときに control_response に載せる、モデルへの理由。クライアントは文を選べない。
const formDenyMessage = "The user declined to answer the questions."

// formInvalidMessage は、form にできない AskUserQuestion の要求を、呼び手・UI に知らせる文 (claude は応答を待ち続ける)。
const formInvalidMessage = "claude: AskUserQuestion の形が form にできないため、この質問には答えられない (claude は応答を待ち続けている)"

// pendingForm は、未決の form の、回答に使う値 (質問の順)。
type pendingForm struct {
	fields []formField
}

// formField は、1 つの質問。question は、answers のキー (claude が出した元の文字列。切らない)。
type formField struct {
	key      string
	question string
	multi    bool
}

// questionInput は、AskUserQuestion の input の、読む部分。
type questionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// clampShown は、表示用の文を、max 文字に収める (切ったら末尾を「…」にする)。回答のキー・値には使わない。
func clampShown(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// buildForm は、AskUserQuestion の input から、FormRequested (ContentHash なし) と、回答に使う値を作る。form にできない形 (型の不一致・質問が無い・
// 質問の文が空か重複・選択肢の label が空か長いか重複・フィールドが多い) は error (呼び手が、承認できない要求として、知らせる)。
func buildForm(id, callID string, input json.RawMessage) (v0.FormRequested, *pendingForm, error) {
	var q questionInput
	if err := json.Unmarshal(input, &q); err != nil {
		return v0.FormRequested{}, nil, fmt.Errorf("input が読めない: %w", err)
	}
	form := v0.FormRequested{RequestID: id, CallID: callID, Kind: v0.FormKindQuestion}
	pf := &pendingForm{}
	texts := map[string]bool{}
	for i, qu := range q.Questions {
		if qu.Question == "" || texts[qu.Question] {
			return v0.FormRequested{}, nil, errors.New("質問の文が空か、重複している (answers のキーに使えない)")
		}
		texts[qu.Question] = true
		key := "q" + strconv.Itoa(i)
		fd := v0.FormField{Key: key, Title: clampShown(qu.Header, v0.MaxFormTitleLen), Description: clampShown(qu.Question, v0.MaxFormDescription), Type: v0.FieldSelect, Custom: true}
		if qu.MultiSelect {
			fd.Type = v0.FieldMultiselect
		}
		for _, o := range qu.Options {
			// label は、回答の値になるので切らない (切ると、claude が出した選択肢と違う値を返す)。長ければ、Validate が落とす。
			fd.Options = append(fd.Options, v0.FormOption{Label: o.Label, Value: o.Label, Description: clampShown(o.Description, v0.MaxFormDescription)})
		}
		form.Fields = append(form.Fields, fd)
		pf.fields = append(pf.fields, formField{key: key, question: qu.Question, multi: qu.MultiSelect})
	}
	if err := form.Validate(); err != nil {
		return v0.FormRequested{}, nil, err
	}
	return form, pf, nil
}

// answersInput は、保持した input に answers を足した、updatedInput を作る。回答 (answer) は、保持したフィールドに対してだけ読む: 知らないキー・型の違い (単一・複数)・長さの超過は error。
// 空の回答 (空文字列・空の配列) は、answers に入れない。
func (f *pendingForm) answersInput(input json.RawMessage, answer map[string]v0.FormValue) (json.RawMessage, error) {
	byKey := map[string]formField{}
	for _, fd := range f.fields {
		byKey[fd.key] = fd
	}
	answers := map[string]string{}
	for key, v := range answer {
		fd, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("claude: 回答のキー %q が、質問に無い", key)
		}
		if v.Multi != fd.multi || (!v.Multi && len(v.Values) != 1) || len(v.Values) > v0.MaxFormAnswerValues {
			return nil, fmt.Errorf("claude: 回答 %q の型・個数が、質問と合わない", key)
		}
		for _, s := range v.Values {
			if utf8.RuneCountInString(s) > v0.MaxFormAnswerText {
				return nil, fmt.Errorf("claude: 回答 %q が長い", key)
			}
		}
		if text := strings.Join(v.Values, ", "); text != "" {
			answers[fd.question] = text
		}
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(input, &in); err != nil || in == nil {
		return nil, errors.New("claude: 保持した input がオブジェクトでない")
	}
	for k := range in { // 異形のキー (Answers・ANSWERS・ſ など。Go の読みは simple fold で同一視) は残さない: 利用者の回答でない answers を、claude に渡さない
		if k != "answers" && strings.EqualFold(k, "answers") {
			delete(in, k)
		}
	}
	a, err := marshal(answers)
	if err != nil {
		return nil, err
	}
	in["answers"] = a
	return marshal(in)
}

// formResolved は、form.resolved の data を作る (answer は answered のときだけ)。
func formResolved(by, outcome, requestID string, answer map[string]v0.FormValue) map[string]any {
	d := map[string]any{"by": by, "outcome": outcome, "request_id": requestID}
	if outcome == v0.FormAnswered {
		d["answer"] = answer
		if answer == nil {
			d["answer"] = map[string]v0.FormValue{}
		}
	}
	return d
}
