package blob

import (
	"context"
	"fmt"
	"io"

	"cpgen/internal/domain"
)

func Verify(ctx context.Context, reader io.Reader, expected domain.BlobRef) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	digest, size, err := domain.SumReader(contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return err
	}
	if digest != expected.Digest || size != expected.Size {
		return fmt.Errorf("%w: verified bytes differ", ErrBlobCorrupt)
	}
	return nil
}
