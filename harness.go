package overload

import (
	"context"
	"io/fs"
)

type Reviewer interface {
	Review(context.Context, ReviewSpec, fs.FS) (ReviewResult, error)
}
