package repository

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
