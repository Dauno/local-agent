package sqlite

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// generateID returns a random opaque identifier with the given prefix.
func generateID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return prefix + hex.EncodeToString(b)
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// parseStrictPositiveDecimal accepts a canonical positive decimal integer:
// digits only, no leading zeros, no sign, and within int64 range.
func parseStrictPositiveDecimal(value string) (int64, bool) {
	if value == "" || len(value) > 19 {
		return 0, false
	}
	if value[0] == '0' && len(value) > 1 {
		return 0, false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, false
	}
	return parsed, true
}
