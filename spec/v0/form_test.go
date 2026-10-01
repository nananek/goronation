package v0

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sampleForm() FormRequested {
	return FormRequested{
		RequestID: "r1", CallID: "c1", Kind: FormKindQuestion, Title: "Questions",
		Fields: []FormField{
			{Key: "q0", Title: "Color", Description: "Which color?", Type: FieldSelect, Required: true,
				Options: []FormOption{{Label: "Red", Value: "Red"}, {Label: "Blue", Value: "Blue"}}},
			{Key: "q1", Title: "Sizes", Type: FieldMultiselect, Custom: true,
				Options: []FormOption{{Label: "S", Value: "S"}, {Label: "M", Value: "M"}}},
			{Key: "q2", Title: "Note", Type: FieldText},
		},
	}
}

func TestFormRequestedValidateAcceptsAWellFormedRequest(t *testing.T) {
	if err := sampleForm().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFormRequestedValidateRejects(t *testing.T) {
	long := func(n int) string { return strings.Repeat("あ", n) }
	cases := map[string]func(f *FormRequested){
		"request_id が空":       func(f *FormRequested) { f.RequestID = "" },
		"kind が空":             func(f *FormRequested) { f.Kind = "" },
		"kind が長い":            func(f *FormRequested) { f.Kind = long(MaxFormKindLen + 1) },
		"フィールドが 0":            func(f *FormRequested) { f.Fields = nil },
		"フィールドが多い":            func(f *FormRequested) { f.Fields = make([]FormField, MaxFormFields+1) },
		"key が重複":             func(f *FormRequested) { f.Fields[1].Key = "q0" },
		"key が空":              func(f *FormRequested) { f.Fields[0].Key = "" },
		"type が不明":            func(f *FormRequested) { f.Fields[0].Type = "password" },
		"select に options なし": func(f *FormRequested) { f.Fields[0].Options = nil },
		"options が多い": func(f *FormRequested) {
			f.Fields[0].Options = make([]FormOption, MaxFormOptions+1)
		},
		"value が重複":                func(f *FormRequested) { f.Fields[0].Options[1].Value = "Red" },
		"value が空":                 func(f *FormRequested) { f.Fields[0].Options[0].Value = "" },
		"label が長い":                func(f *FormRequested) { f.Fields[0].Options[0].Label = long(MaxFormLabelLen + 1) },
		"description が長い":          func(f *FormRequested) { f.Fields[0].Description = long(MaxFormDescription + 1) },
		"text に options がある":       func(f *FormRequested) { f.Fields[2].Options = []FormOption{{Label: "x", Value: "x"}} },
		"title が長い":                func(f *FormRequested) { f.Title = long(MaxFormTitleLen + 1) },
		"option の description が長い": func(f *FormRequested) { f.Fields[0].Options[0].Description = long(MaxFormDescription + 1) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			f := sampleForm()
			mut(&f)
			err := f.Validate()
			if err == nil || !errors.Is(err, ErrInvalidForm) {
				t.Fatalf("Validate = %v, want ErrInvalidForm", err)
			}
		})
	}
}

func TestFormValueJSON(t *testing.T) {
	b, _ := json.Marshal(map[string]FormValue{"a": Text("x"), "b": Many("y", "z"), "c": Many()})
	if string(b) != `{"a":"x","b":["y","z"],"c":[]}` {
		t.Fatalf("Marshal = %s", b)
	}
	var m map[string]FormValue
	if err := json.Unmarshal([]byte(`{"a":"x","b":["y","z"]}`), &m); err != nil {
		t.Fatal(err)
	}
	if m["a"].Multi || m["a"].Values[0] != "x" || !m["b"].Multi || len(m["b"].Values) != 2 {
		t.Fatalf("Unmarshal = %+v", m)
	}
	for _, bad := range []string{`{"a":1}`, `{"a":null}`, `{"a":{"x":1}}`, `{"a":[1]}`, `{"a":true}`,
		`{"a":["x",null]}`, `{"a":[null]}`, `{"a":["x",1]}`, `{"a":["x",true]}`, `{"a":["x",["y"]]}`, `{"a":["x",{"k":"v"}]}`} {
		var m map[string]FormValue
		if err := json.Unmarshal([]byte(bad), &m); err == nil {
			t.Errorf("%s: 通ってはいけない", bad)
		}
	}
	if _, err := json.Marshal(FormValue{Values: []string{"a", "b"}}); err == nil {
		t.Error("単一の回答に 2 つの値が通った")
	}
}

func TestFormResolveValidate(t *testing.T) {
	req := sampleForm()
	ok := FormResolve{RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{
		"q0": Text("Red"), "q1": Many("S", "自由記述"), "q2": Text("メモ")}}
	if err := ok.Validate(req); err != nil {
		t.Fatal(err)
	}
	if err := (FormResolve{RequestID: "r1", Outcome: FormCancelled}).Validate(req); err != nil {
		t.Fatal(err)
	}
	bad := map[string]FormResolve{
		"別の request_id":      {RequestID: "r2", Outcome: FormCancelled},
		"outcome が不明":        {RequestID: "r1", Outcome: "approved"},
		"cancelled に answer": {RequestID: "r1", Outcome: FormCancelled, Answer: map[string]FormValue{"q0": Text("Red")}},
		"未知のキー":              {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "zz": Text("x")}},
		"select に配列":         {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Many("Red")}},
		"multiselect に文字列":   {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "q1": Text("S")}},
		"custom でない select":  {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Green")}},
		"必須が無い":              {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q1": Many("S")}},
		"必須が空文字":             {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("")}},
		"multiselect に空文字列":  {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "q1": Many("S", "")}},
		"multiselect の重複":    {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "q1": Many("S", "S")}},
		"値が長い":               {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "q2": Text(strings.Repeat("あ", MaxFormAnswerText+1))}},
		"値が多い": {RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"),
			"q1": Many(manyStrings(MaxFormAnswerValues + 1)...)}},
	}
	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			if err := r.Validate(req); err == nil || !errors.Is(err, ErrInvalidForm) {
				t.Fatalf("Validate = %v, want ErrInvalidForm", err)
			}
		})
	}
}

func manyStrings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("v", 1) + string(rune('A'+i%26)) + strings.Repeat("x", i/26)
	}
	return out
}

// 回答の検査は、保持した要求に対してだけ行う: 要求に無い値は、クライアントが何を送っても通らない (custom でない限り)。
func TestFormResolveIsCheckedAgainstTheRetainedRequestOnly(t *testing.T) {
	req := sampleForm()
	req.Fields[1].Custom = false // q1 は options だけ
	r := FormResolve{RequestID: "r1", Outcome: FormAnswered, Answer: map[string]FormValue{"q0": Text("Red"), "q1": Many("S", "XL")}}
	if err := r.Validate(req); err == nil {
		t.Fatal("options に無い値が、custom でないフィールドに通った")
	}
}

// 必須のフィールドは、「空でない値が 1 つ以上」: custom の multiselect が、空文字列 1 つで必須を満たさない。
func TestRequiredMeansAtLeastOneNonEmptyValue(t *testing.T) {
	req := FormRequested{RequestID: "r", Kind: "k", Fields: []FormField{
		{Key: "m", Type: FieldMultiselect, Custom: true, Required: true, Options: []FormOption{{Label: "a", Value: "a"}}},
		{Key: "s", Type: FieldSelect, Custom: true, Required: true, Options: []FormOption{{Label: "a", Value: "a"}}},
	}}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, ans := range map[string]map[string]FormValue{
		"multiselect が空配列":     {"m": Many(), "s": Text("a")},
		"multiselect が [\"\"]": {"m": Many(""), "s": Text("a")},
		"select が空文字列":         {"m": Many("a"), "s": Text("")},
		"キーが無い":                {"s": Text("a")},
	} {
		if err := (FormResolve{RequestID: "r", Outcome: FormAnswered, Answer: ans}).Validate(req); err == nil {
			t.Errorf("%s: 必須を満たしたことになった", name)
		}
	}
	ok := FormResolve{RequestID: "r", Outcome: FormAnswered, Answer: map[string]FormValue{"m": Many("自由記述"), "s": Text("a")}}
	if err := ok.Validate(req); err != nil {
		t.Fatal(err)
	}
}

// ContentHash は、この検査の対象でない (呼び手が、別に VerifyHash で照合する)。
func TestFormResolveValidateIgnoresContentHash(t *testing.T) {
	req := sampleForm()
	r := FormResolve{RequestID: "r1", Outcome: FormCancelled, ContentHash: "sha256:00"}
	if err := r.Validate(req); err != nil {
		t.Fatal(err)
	}
	if err := VerifyHash(req.Hash(), r.ContentHash, false); err == nil {
		t.Fatal("VerifyHash が、違うハッシュを通した")
	}
}
