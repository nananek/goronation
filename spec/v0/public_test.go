package v0

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestPublicDropsRawOfGoldenFrames は、golden fixtures の各フレームを Raw に載せた Envelope を、Public() で
// 落とした JSON に、Raw の中の機微な値 (path・cwd・エージェントの ID) も、"raw" というキーも、出ないことを確認する。
// Data は、意図して UI に出す値なので、機微な値を含めない (Data の中身の検査は、アダプタの側の仕事)。
func TestPublicDropsRawOfGoldenFrames(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "testdata", "golden", "*", "*.*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures が見つからない: %v", err)
	}
	canaries := []string{"/work", "session_id", "cwd"}
	frames := 0
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			if !json.Valid(line) {
				continue // capture.sh 以外の説明など。フレームではない
			}
			env := Envelope{V: Version, ID: "e1", TS: "2026-01-01T00:00:00.000Z", Session: "s1", Seq: 1, Type: TypeAgentFrame,
				Data: json.RawMessage(`{}`), Raw: line}
			out, err := json.Marshal(env.Public())
			if err != nil {
				t.Fatal(err)
			}
			frames++
			if strings.Contains(string(out), `"raw"`) {
				t.Errorf("%s: Public() に raw のキーが出た: %s", path, out)
			}
			for _, c := range canaries {
				if bytes.Contains(line, []byte(c)) && bytes.Contains(out, []byte(c)) {
					t.Errorf("%s: Raw の機微な値 %q が Public() に出た: %s", path, c, out)
				}
			}
		}
	}
	if frames == 0 {
		t.Fatal("検査したフレームが 0 件")
	}
}

// TestPublicCarriesEverythingButRaw は、Envelope の Raw 以外の全フィールドが、Public() で写ることを確認する。
// Envelope の各フィールドに、reflect で一意な非ゼロ値を注入して比べるので、フィールドを足したとき、
// UIEnvelope に足し忘れても、Public() でのコピーを書き忘れても、赤になる (手書きのリテラルだと、
// 新しいフィールドがゼロ値のまま一致してしまう)。値を注入できない型のフィールドが増えたときは、fill を直す。
func TestPublicCarriesEverythingButRaw(t *testing.T) {
	et, ut := reflect.TypeOf(Envelope{}), reflect.TypeOf(UIEnvelope{})
	if _, ok := ut.FieldByName("Raw"); ok {
		t.Fatal("UIEnvelope に Raw がある")
	}
	if ut.NumField() != et.NumField()-1 {
		t.Fatalf("UIEnvelope のフィールド数 = %d, want %d (Envelope から Raw を除いたもの)", ut.NumField(), et.NumField()-1)
	}
	var env Envelope
	ev := reflect.ValueOf(&env).Elem()
	for i := 0; i < et.NumField(); i++ {
		n := i + 1
		f := ev.Field(i)
		switch {
		case f.Type() == reflect.TypeOf(json.RawMessage(nil)):
			f.Set(reflect.ValueOf(json.RawMessage(`{"f":` + strconv.Itoa(n) + `}`)))
		case f.Kind() == reflect.String:
			f.SetString("field_" + strconv.Itoa(n))
		case f.CanInt():
			f.SetInt(int64(n))
		case f.CanUint():
			f.SetUint(uint64(n))
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		default:
			t.Fatalf("%s (%s): 値を注入できない型。このテストの fill を直す", et.Field(i).Name, f.Type())
		}
	}
	got := reflect.ValueOf(env.Public())
	for i := 0; i < et.NumField(); i++ {
		name := et.Field(i).Name
		if name == "Raw" {
			continue
		}
		g := got.FieldByName(name)
		if !g.IsValid() {
			t.Errorf("%s: UIEnvelope に無い", name)
			continue
		}
		if !reflect.DeepEqual(ev.Field(i).Interface(), g.Interface()) {
			t.Errorf("%s: %v -> %v", name, ev.Field(i).Interface(), g.Interface())
		}
	}
	// JSON のキーも、Envelope から "raw" を除いたものと一致する。
	keys := func(v any) map[string]bool {
		b, _ := json.Marshal(v)
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	ek, uk := keys(env), keys(env.Public())
	delete(ek, "raw")
	if !reflect.DeepEqual(ek, uk) {
		t.Errorf("JSON のキー: Envelope-raw = %v, UIEnvelope = %v", ek, uk)
	}
}
