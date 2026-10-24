// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"crypto/rand"
	"time"
)

// crockford is ULID's base32 alphabet, lower-cased: a run id is 26 chars of
// [0-9a-z], which is a valid Kubernetes name, workload name and topic key.
const crockford = "0123456789abcdefghjkmnpqrstvwxyz"

// NewRunID is a ULID (48-bit ms timestamp + 80 random bits) in lower case.
func NewRunID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	// 128 bits → 26 base32 chars (the first char carries 3 bits).
	var out [26]byte
	var acc uint64
	bits := 0
	j := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 && j >= 0 {
			out[j] = crockford[acc&31]
			acc >>= 5
			bits -= 5
			j--
		}
	}
	for j >= 0 {
		out[j] = crockford[acc&31]
		acc >>= 5
		j--
	}
	return string(out[:])
}
