package util

import (
	"crypto/md5"
	"encoding/hex"
)

func ToHash(args []string) string {
	hasher := md5.New()

	for _, arg := range args {
		hasher.Write([]byte(arg))
	}

	return hex.EncodeToString(hasher.Sum(nil))
}
