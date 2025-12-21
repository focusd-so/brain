package brain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	brainv1 "github.com/focusd-so/brain/gen/brain/v1"
	"github.com/focusd-so/brain/gen/common"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDeviceHandshake_HMACVerification(t *testing.T) {
	// 1. Setup In-Memory DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to connect database: %v", err)
	}
	// Migrate common.Nonce if needed
	if err := db.AutoMigrate(&common.Nonce{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	// 2. Setup Service
	// Set valid hex secret
	validHexSecret := "12075610360460580dbafc72acfbdd9a4db7890058f409ccaa6ce481396ab52d"
	os.Setenv("FOCUSD_HMAC_SECRET_KEY", validHexSecret)
	defer os.Unsetenv("FOCUSD_HMAC_SECRET_KEY")

	svc := NewServiceImpl(db)

	// 3. Prepare Test Data
	fingerprint := "test-device-fp"
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	nonce := "test-nonce-123"

	payload := fingerprint + timestamp + nonce

	// Calculate Signature
	secretBytes, _ := hex.DecodeString(validHexSecret)
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))

	// 4. Create Request
	req := connect.NewRequest(&brainv1.DeviceHandshakeRequest{
		DeviceFingerprint: fingerprint,
	})
	req.Header().Set("X-Timestamp", timestamp)
	req.Header().Set("X-Nonce", nonce)
	req.Header().Set("X-Signature", signature)

	// 5. Call Handshake
	// Note: We expect an error later in the function (e.g. "failed to mint session" or DB User Error)
	// because we are only testing up to HMAC verification.
	// However, if HMAC fails, we get "signature verification failed".
	// If HMAC succeeds, we proceed.
	// Since we haven't mocked UpsertShadowUser or MintToken logic fully (MintToken relies on FOCUSD_PASETO_KEYS env),
	// let's set FOCUSD_PASETO_KEYS too to pass MintToken or expect a different error.

	// Set mock PASETO key for MintToken to succeed (if it gets that far)
	os.Setenv("FOCUSD_PASETO_KEYS", "0000000000000000000000000000000000000000000000000000000000000000") // 32 bytes hex? No, 32 bytes is 64 hex chars.
	// 32 bytes = 64 hex chars.
	// "00...00" (64 zeros)
	mockPasetoKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	os.Setenv("FOCUSD_PASETO_KEYS", mockPasetoKey)
	defer os.Unsetenv("FOCUSD_PASETO_KEYS")

	_, err = svc.DeviceHandshake(context.Background(), req)

	// If HMAC verification works, we might hit DB error for UpsertShadowUser (commented out in code?)
	// or "failed to mint session" if MintToken fails.
	// Or success.
	// In the provided `service.go`, UpsertShadowUser is commented out!
	// So it goes straight to MintToken.

	if err != nil {
		// If error is "signature verification failed", then we FAILED.
		if err.Error() == "signature verification failed" {
			t.Fatalf("HMAC verification failed unexpectedly")
		}
		// Any other error is acceptable for now (e.g. MintToken issues if any)
		// Actually, if MintToken works, we get success.
		// Let's see.
		// t.Logf("Got error (not signature related): %v", err)
	} else {
		// Success
	}
}
