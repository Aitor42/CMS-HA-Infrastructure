// Package phases defines the contract shared by the deployment phases.
package phases

import "context"

// Phase is one step of the deployment pipeline.
type Phase interface {
	Name() string
	Description() string
	Run(ctx context.Context) error
}
