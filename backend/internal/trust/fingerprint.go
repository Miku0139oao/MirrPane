package trust

// ValidFingerprint checks the canonical SHA-256 pairing-key identifier accepted
// by the local approval API and persisted trust store.
func ValidFingerprint(fingerprint string) bool { return validFingerprint(fingerprint) }
