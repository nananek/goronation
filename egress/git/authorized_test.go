package git

import "testing"

func TestAuthorizedRoute(t *testing.T) {
	p, err := NewPolicy(Repo{"o", "r"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	route, err := p.AuthorizedRoute("POST", PathPrefix+"o/r.git/git-receive-pack")
	if err != nil || route.Repo != (Repo{"o", "r"}) || route.Op != OpReceivePack {
		t.Fatalf("%+v %v", route, err)
	}
	if _, err := p.AuthorizedRoute("POST", PathPrefix+"x/y.git/git-receive-pack"); Reason(err) != CodeRepoNotAllowed {
		t.Fatalf("別の repo を通した: %v", err)
	}
	if _, err := p.AuthorizedRoute("GET", PathPrefix+"o/r.git/git-upload-pack"); Reason(err) != CodeBadRoute {
		t.Fatalf("経路の誤りが、CheckRepo の前に断られない: %v", err)
	}
	// fetch (読み) も、同じ repo だけ。
	if _, err := p.AuthorizedRoute("GET", PathPrefix+"x/y.git/info/refs?service=git-upload-pack"); Reason(err) != CodeRepoNotAllowed {
		t.Fatalf("別の repo への fetch を通した: %v", err)
	}
	// 大文字小文字違いは、同じ repo として通る (Repo.Equal)。
	if _, err := p.AuthorizedRoute("POST", PathPrefix+"O/R.git/git-receive-pack"); err != nil {
		t.Fatalf("大文字小文字違いは通るはず: %v", err)
	}
}

// FuzzAuthorizedRoute は、AuthorizedRoute が、素の ParseRoute の結果 (通す・断るの理由) を、CheckRepo で絞るだけで、
// それ以外の判定を変えないこと・許可外の repo を 1 件も通さないことを、任意の入力で確かめる。
func FuzzAuthorizedRoute(f *testing.F) {
	p, err := NewPolicy(Repo{"o", "r"}, sess)
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{
		PathPrefix + "o/r.git/git-receive-pack", PathPrefix + "o/r.git/info/refs?service=git-upload-pack",
		PathPrefix + "x/y.git/git-receive-pack", PathPrefix + "O/R.git/git-upload-pack", "",
	} {
		f.Add("POST", s)
		f.Add("GET", s)
	}
	f.Fuzz(func(t *testing.T, method, target string) {
		route, err := p.AuthorizedRoute(method, target)
		bare, bareErr := ParseRoute(method, target)
		switch {
		case bareErr != nil:
			if err == nil || Reason(err) != Reason(bareErr) {
				t.Fatalf("ParseRoute が断るのに、AuthorizedRoute が違う結果: bare=%v got=%v", bareErr, err)
			}
		case !p.Repo.Equal(bare.Repo):
			if err == nil || Reason(err) != CodeRepoNotAllowed {
				t.Fatalf("許可外の repo %+v を、CodeRepoNotAllowed 以外で扱った: %v", bare.Repo, err)
			}
		default:
			if err != nil || route != bare {
				t.Fatalf("許可内なのに、断った・食い違った: route=%+v bare=%+v err=%v", route, bare, err)
			}
		}
	})
}
