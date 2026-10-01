package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

const recallResponseIdentityLimit = 8 << 20

// These are byte identities, not evidence of a valid terminal or task success.
// The digest contains no retained response text. Only qualified Step requests
// in an explicitly enabled observation instance enter this path.
type recallResponseIdentity struct {
	digest      hash.Hash
	bytes       int64
	limited     bool
	writeFailed bool
}

func (q *contextRecallRequest) observeResponseWrite(data []byte, n int, err error) {
	if q == nil || !q.event.ToolsEligible {
		return
	}
	if q.responseIdentity == nil {
		q.responseIdentity = &recallResponseIdentity{digest: sha256.New()}
	}
	id := q.responseIdentity
	if err != nil || n != len(data) {
		id.writeFailed = true
	}
	if n < 0 || n > len(data) {
		id.limited = true
		id.digest = nil
		return
	}
	id.bytes += int64(n)
	if id.bytes > recallResponseIdentityLimit {
		id.limited = true
		id.digest = nil
	}
	if id.digest != nil {
		_, _ = id.digest.Write(data[:n])
	}
}

func (q *contextRecallRequest) finishResponseIdentity() {
	if q == nil || q.responseIdentity == nil {
		return
	}
	id := q.responseIdentity
	q.event.ResponseBodyBytes = id.bytes
	q.event.ResponseHashLimited = id.limited
	q.event.ResponseWriteFailed = id.writeFailed
	q.event.ResponseRequestCanceled = q.ctx != nil && q.ctx.Err() != nil
	if id.digest != nil {
		q.event.ResponseBodySHA256 = hex.EncodeToString(id.digest.Sum(nil))
	}
}
