package airplayd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
	"github.com/miku0139/airplay-go/backend/internal/trust"
)

// The UI's resetIdentity deletes config.json and restarts airplayd. Exercise
// that exact persistence lifecycle without changing config.go or using sockets.
func TestUIIdentityResetAlsoClearsSeparateTrustStore(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := pairing.KeysFromSeed(mustSeed(cfg))
	if err != nil {
		t.Fatal(err)
	}
	store, err := trust.Open(dir, keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	sender := trust.Fingerprint([]byte("formerly paired sender"))
	if err := store.TrustSender(sender); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, config.FileName)); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	resetKeys, err := pairing.KeysFromSeed(mustSeed(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if trust.Fingerprint(keys.PublicKey()) == trust.Fingerprint(resetKeys.PublicKey()) {
		t.Fatal("identity reset reused the old key")
	}
	store, err = trust.Open(dir, resetKeys.PublicKey())
	if err != nil || store.IsTrustedSender(sender) {
		t.Fatalf("deleting config.json retained old sender trust: %v", err)
	}
	newSender := trust.Fingerprint([]byte("new sender"))
	if store.IsTrustedSender(newSender) {
		t.Fatal("identity reset implicitly trusted a new sender")
	}
	if err := store.TrustSender(newSender); err != nil {
		t.Fatal(err)
	}
}
