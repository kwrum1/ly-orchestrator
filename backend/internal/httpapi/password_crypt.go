package httpapi

import (
	"crypto/sha512"
	"crypto/subtle"
	"fmt"
	"strings"
)

// The bootstrap credential may be provisioned either as the product-native
// verifier (sha256 of username NUL password, used by the installer's
// control-admin.hash) or as a glibc SHA-512 crypt hash ($6$) from
// /etc/ly-route/installer-admin.hash. Both are one-way; the plaintext password
// never needs to be written to the target disk.

const (
	cryptRounds = 5000
	cryptITOA64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

func verifyPasswordHash(encoded, username, password string) bool {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return false
	}
	if strings.HasPrefix(encoded, "sha256:") {
		return constantEqual(encoded, hashPassword(username, password))
	}
	if strings.HasPrefix(encoded, "$6$") {
		return verifySHA512Crypt(encoded, password)
	}
	return false
}

func verifySHA512Crypt(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) < 4 || parts[1] != "6" {
		return false
	}
	salt := parts[2]
	rounds := cryptRounds
	if strings.HasPrefix(salt, "rounds=") {
		if _, err := fmt.Sscanf(strings.TrimPrefix(salt, "rounds="), "%d", &rounds); err != nil {
			return false
		}
		if len(parts) < 5 {
			return false
		}
		salt = parts[3]
	}
	salt = cryptTruncateSalt(salt)
	computed := sha512Crypt(password, salt, rounds)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(encoded)) == 1
}

func cryptTruncateSalt(salt string) string {
	cleaned := make([]byte, 0, 16)
	for i := 0; i < len(salt) && len(cleaned) < 16; i++ {
		if strings.IndexByte(cryptITOA64, salt[i]) >= 0 {
			cleaned = append(cleaned, salt[i])
		}
	}
	return string(cleaned)
}

// sha512Crypt implements Ulrich Drepper's SHA-512 crypt (glibc compatible).
func sha512Crypt(password, salt string, rounds int) string {
	if rounds < 1000 {
		rounds = 1000
	}
	if rounds > 999999999 {
		rounds = 999999999
	}
	salt = cryptTruncateSalt(salt)
	key := []byte(password)
	saltBytes := []byte(salt)

	alternate := sha512.New()
	alternate.Write(key)
	alternate.Write(saltBytes)
	alternate.Write(key)
	altResult := alternate.Sum(nil)

	digest := sha512.New()
	digest.Write(key)
	digest.Write(saltBytes)
	for remaining := len(key); remaining > 0; remaining -= 64 {
		count := remaining
		if count > 64 {
			count = 64
		}
		digest.Write(altResult[:count])
	}
	for bits := len(key); bits > 0; bits >>= 1 {
		if bits&1 == 1 {
			digest.Write(altResult)
		} else {
			digest.Write(key)
		}
	}
	temp := digest.Sum(nil)

	digest.Reset()
	for i := 0; i < len(key); i++ {
		digest.Write(key)
	}
	dp := digest.Sum(nil)
	pBytes := make([]byte, 0, len(key))
	for remaining := len(key); remaining > 0; remaining -= 64 {
		count := remaining
		if count > 64 {
			count = 64
		}
		pBytes = append(pBytes, dp[:count]...)
	}

	digest.Reset()
	for i := 0; i < 16+int(temp[0]); i++ {
		digest.Write(saltBytes)
	}
	ds := digest.Sum(nil)
	sBytes := make([]byte, 0, len(saltBytes))
	for remaining := len(saltBytes); remaining > 0; remaining -= 64 {
		count := remaining
		if count > 64 {
			count = 64
		}
		sBytes = append(sBytes, ds[:count]...)
	}

	result := temp
	for i := 0; i < rounds; i++ {
		digest.Reset()
		if i&1 == 1 {
			digest.Write(pBytes)
		} else {
			digest.Write(result)
		}
		if i%3 != 0 {
			digest.Write(sBytes)
		}
		if i%7 != 0 {
			digest.Write(pBytes)
		}
		if i&1 == 1 {
			digest.Write(result)
		} else {
			digest.Write(pBytes)
		}
		result = digest.Sum(nil)
	}

	var builder strings.Builder
	builder.WriteString("$6$")
	if rounds != cryptRounds {
		fmt.Fprintf(&builder, "rounds=%d$", rounds)
	}
	builder.WriteString(salt)
	builder.WriteByte('$')
	encodeCrypt64(&builder, result)
	return builder.String()
}

func encodeCrypt64(builder *strings.Builder, sum []byte) {
	order := [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45},
		{25, 46, 4}, {47, 5, 26}, {6, 27, 48}, {28, 49, 7},
		{50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32},
		{12, 33, 54}, {34, 55, 13}, {56, 14, 35}, {15, 36, 57},
		{37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
		{62, 20, 41},
	}
	for _, triple := range order {
		crypt64(builder, sum[triple[0]], sum[triple[1]], sum[triple[2]], 4)
	}
	crypt64(builder, 0, 0, sum[63], 2)
}

func crypt64(builder *strings.Builder, b2, b1, b0 byte, count int) {
	value := uint32(b2)<<16 | uint32(b1)<<8 | uint32(b0)
	for i := 0; i < count; i++ {
		builder.WriteByte(cryptITOA64[value&0x3f])
		value >>= 6
	}
}
