package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parsePorts は、"3128,8080" の形の、1 つ以上のポート (1〜65535。重複は 1 つにまとめる) を返す。
func parsePorts(s string) ([]int, error) {
	if s == "" {
		return nil, errors.New("--allow-connect に、1 つ以上のポートが要る")
	}
	var out []int
	seen := map[int]bool{}
	for _, f := range strings.Split(s, ",") {
		p, err := strconv.Atoi(f)
		if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != f {
			return nil, fmt.Errorf("--allow-connect のポートが不正: %q (1〜65535 の 10 進数)", f)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}
