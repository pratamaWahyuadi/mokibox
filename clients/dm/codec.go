package dm

import (
	"encoding/base64"
	"fmt"
)

const (
	maxCiphertextBytes = 16 << 10
	maxDecodedBytes    = 1 << 20
)

func decodeBase64(value string, expected int) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("empty base64 value")
	}
	if len(value) > base64.RawStdEncoding.EncodedLen(maxDecodedBytes) {
		return nil, fmt.Errorf("base64 value is too large")
	}
	encodings := []*base64.Encoding{base64.RawStdEncoding.Strict(), base64.StdEncoding.Strict()}
	for _, enc := range encodings {
		decoded, err := enc.DecodeString(value)
		if err == nil {
			if expected > 0 && len(decoded) != expected {
				return nil, fmt.Errorf("expected %d bytes, got %d", expected, len(decoded))
			}
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("invalid base64 value")
}

func encodeBase64(value []byte) string {
	return base64.RawStdEncoding.EncodeToString(value)
}

func normalizeBase64(value string, expected int) (string, error) {
	decoded, err := decodeBase64(value, expected)
	if err != nil {
		return "", err
	}
	return encodeBase64(decoded), nil
}
