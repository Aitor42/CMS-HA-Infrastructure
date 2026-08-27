package phases

import "context"

type Phase interface {
	Name() string
	Description() string
	Run(ctx context.Context) error
}
