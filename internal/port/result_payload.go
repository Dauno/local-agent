package port

import (
	"context"
	"errors"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var ErrResultPayloadConflict = errors.New("result payload conflicts with reserved storage")

// ResultPayloadStore owns private bytes for a V2 result. Its storage binding is
// deterministic from the catalog-assigned opaque result ID so a retry can
// verify a published payload without replaying its producer.
type ResultPayloadStore interface {
	StorageFor(resultID string) (domain.ResultStorage, error)
	Publish(context.Context, domain.ResultStorage, string) error
	Verify(context.Context, domain.ResultStorage, string, int64) error
	ReadRange(context.Context, domain.ResultStorage, string, int64, int64, int64) (domain.ResultChunk, error)
}
