package routing

import (
	"crypto/sha256"
	"fmt"
)

func contentHash(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
