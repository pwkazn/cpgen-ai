package transfer

import "context"

func Keep(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	<-ctx.Done()
	return nil
}
