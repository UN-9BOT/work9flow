package storage

import (
	"crypto/rand"
	"encoding/hex"
)

func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "x"
	}
	return hex.EncodeToString(b[:])
}
