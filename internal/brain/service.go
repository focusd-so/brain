package brain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"gorm.io/gorm"

	brainv1 "github.com/focusd-so/brain/gen/brain/v1"
	"github.com/focusd-so/brain/gen/brain/v1/brainv1connect"
	"github.com/focusd-so/brain/gen/common"
	"github.com/focusd-so/brain/internal/auth"
)

type ServiceImpl struct {
	gormDB *gorm.DB
}

func NewServiceImpl(gormDB *gorm.DB) *ServiceImpl {
	return &ServiceImpl{gormDB: gormDB}
}

var _ brainv1connect.BrainServiceHandler = (*ServiceImpl)(nil)

func (s *ServiceImpl) DeviceHandshake(ctx context.Context, req *connect.Request[brainv1.DeviceHandshakeRequest]) (*connect.Response[brainv1.DeviceHandshakeResponse], error) {
	// ---------------------------------------------------------
	// STEP 1: VERIFY HMAC SIGNATURE (App Attestation)
	// ---------------------------------------------------------
	// We do this manually here because Handshake is a public endpoint
	// and doesn't use the standard AuthInterceptor.
	if err := s.verifyHMAC(req); err != nil {
		slog.Error("failed to verify hmac", "error", err)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("signature verification failed"))
	}

	fingerprint := req.Msg.DeviceFingerprint
	if fingerprint == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("fingerprint required"))
	}

	// ---------------------------------------------------------
	// STEP 2: FIND OR CREATE SHADOW USER
	// ---------------------------------------------------------

	user, err := s.upsertShadowUser(ctx, fingerprint)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("db error: %w", err))
	}

	// ---------------------------------------------------------
	// STEP 3: MINT PASETO TOKEN
	// ---------------------------------------------------------

	sessionToken, err := auth.MintToken(user.Id, user.Role)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("failed to mint session"))
	}

	// ---------------------------------------------------------
	// STEP 4: RETURN RESPONSE
	// ---------------------------------------------------------

	return connect.NewResponse(&brainv1.DeviceHandshakeResponse{
		Token: sessionToken,
	}), nil
}

func (s *ServiceImpl) verifyHMAC(req *connect.Request[brainv1.DeviceHandshakeRequest]) error {
	timestampStr := req.Header().Get("X-Timestamp")
	nonce := req.Header().Get("X-Nonce")
	signature := req.Header().Get("X-Signature")

	if timestampStr == "" || signature == "" {
		return errors.New("missing security headers")
	}

	// Replay Attack Check (Timestamp window: 30 seconds)
	ts, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return errors.New("invalid timestamp")
	}

	now := time.Now().Unix()
	if now-ts > 30 || ts-now > 30 {
		return errors.New("request expired")
	}

	// Replay Attack Check (Nonce)
	if err := s.gormDB.Where("nonce = ?", nonce).First(&common.Nonce{}).Error; err != nil {
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("db error: %w", err)
		}
	}

	if err := s.gormDB.Create(&common.Nonce{
		Nonce:     nonce,
		CreatedAt: now,
		ExpiresAt: now + 30,
	}).Error; err != nil {
		return fmt.Errorf("db error: %w", err)
	}

	slog.Info("verifying hmac", "device_fingerprint", req.Msg.DeviceFingerprint, "timestamp", timestampStr, "nonce", nonce, "signature", signature)

	// Reconstruct the String-to-Sign
	// Must match Client Logic EXACTLY: "BodyJson+Timestamp+Nonce"
	// Note: In ConnectRPC, we don't always have raw JSON body easily accessbile
	// in the handler object without middleware.
	// SIMPLIFICATION: Sign the Fingerprint field specifically, not whole JSON.
	payload := req.Msg.DeviceFingerprint + timestampStr + nonce

	// 5. Calculate Expected Hash
	secretStr := os.Getenv("HMAC_SECRET_KEY")
	secret, err := hex.DecodeString(secretStr)
	if err != nil {
		slog.Error("failed to decode hmac secret", "error", err)
		return errors.New("internal server error")
	}
	slog.Info("verifying hmac", "secret_len", len(secret))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expectedSignature := hex.EncodeToString(mac.Sum(nil))

	// 6. Compare (Constant Time to prevent Timing Attacks)
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return errors.New("invalid signature")
	}

	return nil
}

func (s *ServiceImpl) upsertShadowUser(ctx context.Context, fingerprint string) (*common.User, error) {
	var user common.User
	err := s.gormDB.Where("device_fingerprint_hash = ?", fingerprint).First(&user).Error
	if err == nil {
		return &user, nil
	}

	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	// Create new user
	newUser := common.User{
		DeviceFingerprintHash: fingerprint,
		Role:                  "anonymous",
		OsInfo:                "unknown", // TODO: Populate from request?
		CreatedAt:             time.Now().Unix(),
	}

	if err := s.gormDB.Create(&newUser).Error; err != nil {
		return nil, err
	}

	return &newUser, nil
}
