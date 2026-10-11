// Package randutil holds random helpers shared by the dialer and listener.
package randutil

import (
	crand "crypto/rand"
	"math/big"
)

// RandBetween returns a uniformly random int64 in [from, to).
//
// If from == to it returns from. If from > to the bounds are swapped.
func RandBetween(from, to int64) int64 {
	if from == to {
		return from
	}
	if from > to {
		from, to = to, from
	}
	n, err := crand.Int(crand.Reader, big.NewInt(to-from))
	if err != nil {
		return from
	}
	return from + n.Int64()
}

// RandStringFromCharset returns a string of n characters drawn from charset
// using a rejection-sampling loop so that the distribution stays uniform.
func RandStringFromCharset(n int, charset string) (string, bool) {
	if n <= 0 || len(charset) == 0 {
		return "", false
	}

	m := len(charset)
	limit := byte(256 - (256 % m))

	result := make([]byte, n)
	i := 0

	buf := make([]byte, 256)
	for i < n {
		if _, err := crand.Read(buf); err != nil {
			return "", false
		}
		for _, rb := range buf {
			if rb >= limit {
				continue
			}
			result[i] = charset[int(rb)%m]
			i++
			if i == n {
				break
			}
		}
	}

	return string(result), true
}
