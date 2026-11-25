package dm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const fingerprintPrefix = "mokibox-dm-fingerprint-v1\x00"

func Fingerprint(userID, deviceID, curve25519Key, ed25519Key string) (string, error) {
	user, err := uuid.Parse(userID)
	if err != nil {
		return "", fmt.Errorf("invalid user id: %w", err)
	}
	device, err := uuid.Parse(deviceID)
	if err != nil {
		return "", fmt.Errorf("invalid device id: %w", err)
	}
	curve, err := decodeBase64(curve25519Key, 32)
	if err != nil {
		return "", fmt.Errorf("invalid curve25519 identity key: %w", err)
	}
	ed, err := decodeBase64(ed25519Key, 32)
	if err != nil {
		return "", fmt.Errorf("invalid ed25519 identity key: %w", err)
	}
	userBytes := user[:]
	deviceBytes := device[:]
	payload := make([]byte, 0, len(fingerprintPrefix)+16+16+32+32)
	payload = append(payload, fingerprintPrefix...)
	payload = append(payload, userBytes[:]...)
	payload = append(payload, deviceBytes[:]...)
	payload = append(payload, curve...)
	payload = append(payload, ed...)
	sum := sha256.Sum256(payload)
	return strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func NormalizeFingerprint(value string) string {
	cleaned := strings.ToUpper(strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', ':', '\t', '\n', '\r':
			return -1
		default:
			return r
		}
	}, value))
	return cleaned
}

func FormatFingerprint(normalized string) string {
	cleaned := NormalizeFingerprint(normalized)
	if len(cleaned) == 0 {
		return ""
	}
	groups := make([]string, 0, (len(cleaned)+3)/4)
	for i := 0; i < len(cleaned); i += 4 {
		end := i + 4
		if end > len(cleaned) {
			end = len(cleaned)
		}
		groups = append(groups, cleaned[i:end])
	}
	return strings.Join(groups, " ")
}
