package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/focusd-so/brain/gen/brain/v1/brainv1connect"
	"github.com/o1egl/paseto"
)

// ---------------------------------------------------------
// 1. CONFIGURATION & KEYS
// ---------------------------------------------------------

// KeyManager handles rotation. Keys are stored in env var:
// PASETO_KEYS="HEX_KEY_NEW,HEX_KEY_OLD"
type KeyManager struct{}

func (km KeyManager) GetActiveKey() ([]byte, error) {
	keys := strings.Split(os.Getenv("PASETO_KEYS"), ",")
	if len(keys) == 0 || keys[0] == "" {
		return nil, errors.New("PASETO_KEYS not configured")
	}
	return hex.DecodeString(strings.TrimSpace(keys[0]))
}

func (km KeyManager) GetAllKeys() ([][]byte, error) {
	rawKeys := strings.Split(os.Getenv("PASETO_KEYS"), ",")
	var parsedKeys [][]byte

	for _, k := range rawKeys {
		if k == "" {
			continue
		}
		b, err := hex.DecodeString(strings.TrimSpace(k))
		if err != nil {
			return nil, fmt.Errorf("invalid hex key: %v", err)
		}
		parsedKeys = append(parsedKeys, b)
	}

	if len(parsedKeys) == 0 {
		return nil, errors.New("no valid keys found")
	}
	return parsedKeys, nil
}

// ---------------------------------------------------------
// 2. DATA STRUCTURES
// ---------------------------------------------------------

// UserClaims represents the data inside the encrypted token
type UserClaims struct {
	UserID    int64     `json:"sub"`
	Role      string    `json:"role"` // "anonymous" or "pro"
	ExpiresAt time.Time `json:"exp"`
}

// Valid checks if token is expired
func (c *UserClaims) Valid() error {
	if time.Now().After(c.ExpiresAt) {
		return errors.New("token expired")
	}
	return nil
}

// ---------------------------------------------------------
// 3. CORE FUNCTIONS (Mint & Validate)
// ---------------------------------------------------------

// MintToken creates a new encrypted PASETO token
func MintToken(userID int64, role string) (string, error) {
	km := KeyManager{}
	key, err := km.GetActiveKey()
	if err != nil {
		return "", err
	}

	now := time.Now()
	claims := UserClaims{
		UserID:    userID,
		Role:      role,
		ExpiresAt: now.Add(24 * time.Hour), // 24h Session
	}

	// Sign & Encrypt (v2.local)
	return paseto.NewV2().Encrypt(key, claims, nil)
}

// ValidateToken decrypts the token trying all available keys
func ValidateToken(tokenStr string) (*UserClaims, error) {
	km := KeyManager{}
	keys, err := km.GetAllKeys()
	if err != nil {
		return nil, err
	}

	var claims UserClaims
	var lastErr error

	// Try keys in order (Active -> Old)
	for _, key := range keys {
		err := paseto.NewV2().Decrypt(tokenStr, key, &claims, nil)
		if err == nil {
			// Decrypt success, check expiration
			if expErr := claims.Valid(); expErr != nil {
				return nil, expErr
			}
			return &claims, nil
		}
		lastErr = err
	}

	return nil, fmt.Errorf("invalid token: %v", lastErr)
}

// ---------------------------------------------------------
// 4. CONNECT RPC INTERCEPTORS (The Middleware)
// ---------------------------------------------------------

type authKey struct{}

// authInterceptor implements the connect.Interceptor interface
type authInterceptor struct{}

// WrapUnary implements unary RPC authentication
func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		// 1. Skip Auth for specific public endpoints (like Handshake)
		if req.Spec().Procedure == brainv1connect.BrainServiceDeviceHandshakeProcedure {
			return next(ctx, req)
		}

		// 2. Extract Header
		token := req.Header().Get("Authorization")
		// Standard format: "Bearer v2.local.AAAA..."
		token = strings.TrimPrefix(token, "Bearer ")
		token = strings.TrimSpace(token)

		if token == "" {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
		}

		// 3. Validate PASETO
		claims, err := ValidateToken(token)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired session"))
		}

		// 4. Inject Claims into Context
		ctx = context.WithValue(ctx, authKey{}, claims)

		return next(ctx, req)
	}
}

// WrapStreamingClient is a no-op for server-side interceptors
func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements streaming RPC authentication
func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		// 1. Skip Auth for specific public endpoints (like Handshake)
		if conn.Spec().Procedure == brainv1connect.BrainServiceDeviceHandshakeProcedure {
			return next(ctx, conn)
		}

		// 2. Extract Header
		token := conn.RequestHeader().Get("Authorization")
		// Standard format: "Bearer v2.local.AAAA..."
		token = strings.TrimPrefix(token, "Bearer ")
		token = strings.TrimSpace(token)

		if token == "" {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
		}

		// 3. Validate PASETO
		claims, err := ValidateToken(token)
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired session"))
		}

		// 4. Inject Claims into Context
		ctx = context.WithValue(ctx, authKey{}, claims)

		return next(ctx, conn)
	}
}

// NewAuthInterceptor creates a ConnectRPC interceptor for both unary and streaming
func NewAuthInterceptor() connect.Interceptor {
	return &authInterceptor{}
}

// NewStreamAuthInterceptor is deprecated - use NewAuthInterceptor which handles both
// Kept for backwards compatibility
func NewStreamAuthInterceptor() connect.Interceptor {
	return NewAuthInterceptor()
}

// GetUser extracts user data from context in your API handlers
func GetUser(ctx context.Context) (*UserClaims, bool) {
	u, ok := ctx.Value(authKey{}).(*UserClaims)
	return u, ok
}
