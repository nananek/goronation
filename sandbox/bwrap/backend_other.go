//go:build !linux

package bwrap

import (
	"context"
	"fmt"

	"github.com/nananek/goronation/core/sandbox"
)

// probe は、Linux 以外では、常に ErrNotInstalled を包んだ error を返す (bwrap は Linux だけ)。
func (b *Backend) probe(ctx context.Context) error {
	return fmt.Errorf("%w: bwrap は Linux だけ", sandbox.ErrNotInstalled)
}

// start は、Linux 以外では、常に ErrNotInstalled を包んだ error を返す。
func (b *Backend) start(ctx context.Context, s sandbox.Spec) (sandbox.Cage, error) {
	return nil, fmt.Errorf("%w: bwrap は Linux だけ", sandbox.ErrNotInstalled)
}
