package core

// Rules は、規則の集まり。
type Rules struct {
	Name string // 名前
	// Tags は、タグ。
	Tags   []string
	hidden int
}

// New は、Rules を作る。
func New(name string) *Rules { return &Rules{Name: name} }

// Label は、ラベルを返す。
func (r *Rules) Label() string { return r.Name }

func (r *Rules) unexportedMethod() {}

// Level は、水準を表す。
type Level int

// 水準の定数。
const (
	// Low は、低い水準。
	Low Level = iota
	// High は、高い水準。
	High
	unexported
)

// DefaultName は、既定の名前。
const DefaultName = "default"

// Table は、表。値が複数行にわたる初期値は、文書では "..." になる。
var Table = map[string]int{
	"a": 1,
	"b": 2,
}

// Version は、1 行の初期値なので、そのまま出る。
var Version = "1.0"

var hiddenVar = 1

// Helper は、関数。
func Helper(a, b int) (sum int, err error) { return a + b, nil }

func hiddenFunc() {}

type hiddenType struct{}
