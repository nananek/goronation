package git

// AuthorizedRoute は、ParseRoute と p.CheckRepo を続けて行う、経路の唯一の入口。呼び手が CheckRepo を呼び忘れて、
// 許可外の repo への要求 (fetch も push も) が、経路の検査だけを通ってしまうことを防ぐ (攻撃者視点レビュー L3)。
// 中継する側は、この関数だけを呼び、素の ParseRoute は使わない。
func (p Policy) AuthorizedRoute(method, target string) (Route, error) {
	route, err := ParseRoute(method, target)
	if err != nil {
		return Route{}, err
	}
	if err := p.CheckRepo(route.Repo); err != nil {
		return Route{}, err
	}
	return route, nil
}
