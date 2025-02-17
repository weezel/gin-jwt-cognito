package jwt

import (
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	jwtgo "github.com/golang-jwt/jwt/v4"
	"golang.org/x/crypto/blake2b"
)

var (
	// ErrAuthHeaderEmpty thrown when an empty Authorization header is received
	ErrAuthHeaderEmpty = errors.New("auth header empty")

	// ErrInvalidAuthHeader thrown when an invalid Authorization header is received
	ErrInvalidAuthHeader = errors.New("invalid auth header")
)

const (
	// HeaderAuthenticate the Gin authenticate header
	HeaderAuthenticate = "WWW-Authenticate"

	// HeaderAuthorization the auth header that gets passed to all services
	HeaderAuthorization = "Authorization"

	// Forward slash character
	ForwardSlash = "/"

	// HEADER used by the JWT middle ware
	HEADER = "header"

	// IssuerFieldName the issuer field name
	IssuerFieldName = "iss"
)

// AuthMiddleware middleware
type AuthMiddleware struct {
	// User can define own Unauthorized func.
	Unauthorized func(*gin.Context, int, string)

	Timeout time.Duration

	// TokenLookup the header name of the token
	TokenLookup string

	// TimeFunc
	TimeFunc func() time.Time

	// Realm name to display to the user. Required.
	Realm string

	// to verify issuer
	VerifyIssuer bool

	// Region aws region
	Region string

	// UserPoolID the cognito user pool id
	UserPoolID string

	// The issuer
	Iss string

	// JWK public JSON Web Key (JWK) for your user pool
	JWK map[string]JWKKey
}

// JWK is json data struct for JSON Web Key
type JWK struct {
	Keys []JWKKey
}

// JWKKey is json data struct for cognito jwk key
type JWKKey struct {
	Alg string
	E   string
	Kid string
	Kty string
	N   string
	Use string
}

// AuthError auth error response
type AuthError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// MiddlewareInit initialize jwt configs.
func (mw *AuthMiddleware) MiddlewareInit() {
	if mw.TokenLookup == "" {
		mw.TokenLookup = "header:" + HeaderAuthorization
	}

	if mw.Timeout == 0 {
		mw.Timeout = time.Hour
	}

	if mw.TimeFunc == nil {
		mw.TimeFunc = time.Now
	}

	if mw.Unauthorized == nil {
		mw.Unauthorized = func(c *gin.Context, code int, message string) {
			c.JSON(code, AuthError{Code: code, Message: message})
		}
	}

	if mw.Realm == "" {
		mw.Realm = "gin jwt"
	}
}

func (mw *AuthMiddleware) middlewareImpl(c *gin.Context) {
	// Parse the given token
	var tokenStr string
	var err error

	parts := strings.Split(mw.TokenLookup, ":")
	switch parts[0] {
	case HEADER:
		tokenStr, err = mw.jwtFromHeader(c, parts[1])
	}

	if err != nil {
		log.Printf("JWT token Parser error: %s", err.Error())
		mw.unauthorized(c, http.StatusUnauthorized, err.Error())
		return
	}

	token, err := mw.parse(tokenStr)
	if err != nil {
		log.Printf("JWT token Parser error: %s", err.Error())
		mw.unauthorized(c, http.StatusUnauthorized, err.Error())
		return
	}

	c.Set("JWT_TOKEN", token)
	c.Next()
}

func (mw *AuthMiddleware) jwtFromHeader(c *gin.Context, key string) (string, error) {
	authHeader := c.Request.Header.Get(key)

	if authHeader == "" {
		return "", ErrAuthHeaderEmpty
	}
	return authHeader, nil
}

func (mw *AuthMiddleware) unauthorized(c *gin.Context, code int, message string) {
	if mw.Realm == "" {
		mw.Realm = "gin jwt"
	}
	c.Header(HeaderAuthenticate, "JWT realm="+mw.Realm)
	c.Abort()

	mw.Unauthorized(c, code, message)
}

// MiddlewareFunc implements the Middleware interface.
func (mw *AuthMiddleware) MiddlewareFunc() gin.HandlerFunc {
	// initialise
	mw.MiddlewareInit()
	return func(c *gin.Context) {
		mw.middlewareImpl(c)
	}
}

// AuthJWTMiddleware create an instance of the middle ware function
func AuthJWTMiddleware(iss, userPoolID, region string) (*AuthMiddleware, error) {
	// Download the public json web key for the given user pool ID at the start of the plugin
	jwk, err := getJWK(fmt.Sprintf("https://cognito-idp.%v.amazonaws.com/%v/.well-known/jwks.json",
		region,
		userPoolID,
	))
	if err != nil {
		return nil, err
	}

	authMiddleware := &AuthMiddleware{
		Timeout: time.Hour,

		Unauthorized: func(c *gin.Context, code int, message string) {
			c.JSON(code, AuthError{Code: code, Message: message})
		},

		// Token header
		TokenLookup: "header:" + HeaderAuthorization,
		TimeFunc:    time.Now,
		JWK:         jwk,
		Iss:         iss,
		Region:      region,
		UserPoolID:  userPoolID,
	}
	return authMiddleware, nil
}

func (mw *AuthMiddleware) parse(tokenStr string) (*jwtgo.Token, error) {
	// 1. Decode the token string into JWT format.
	token, err := jwtgo.Parse(tokenStr, func(token *jwtgo.Token) (interface{}, error) {
		// cognito user pool : RS256
		if _, ok := token.Method.(*jwtgo.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		// 5. Get the kid from the JWT token header and retrieve the corresponding JSON Web Key that was stored
		if kid, ok := token.Header["kid"]; ok {
			if kidStr, ok := kid.(string); ok {
				key := mw.JWK[kidStr]
				// 6. Verify the signature of the decoded JWT token.
				rsaPublicKey := convertKey(key.E, key.N)
				return rsaPublicKey, nil
			}
		}

		// rsa public key
		return "", nil
	})
	if err != nil {
		return token, err
	}

	claims := token.Claims.(jwtgo.MapClaims)

	iss, ok := claims["iss"]
	if !ok {
		return token, fmt.Errorf("token does not contain issuer")
	}
	issStr := iss.(string)
	if strings.Contains(issStr, "cognito-idp") {
		err = validateAWSJwtClaims(claims, mw.Region, mw.UserPoolID)
		if err != nil {
			return token, err
		}
	}

	if token.Valid {
		return token, nil
	}
	return token, err
}

// validateAWSJwtClaims validates AWS Cognito User Pool JWT
func validateAWSJwtClaims(claims jwtgo.MapClaims, region, userPoolID string) error {
	var err error
	// 3. Check the iss claim. It should match your user pool.
	issShoudBe := fmt.Sprintf("https://cognito-idp.%v.amazonaws.com/%v", region, userPoolID)
	err = validateClaimItem("iss", []string{issShoudBe}, claims)
	if err != nil {
		Error.Printf("Failed to validate the jwt token claims %v", err)
		return err
	}

	// 4. Check the token_use claim.
	validateTokenUse := func() error {
		if tokenUse, ok := claims["token_use"]; ok {
			if tokenUseStr, ok := tokenUse.(string); ok {
				if tokenUseStr == "id" || tokenUseStr == "access" {
					return nil
				}
			}
		}
		return errors.New("token_use should be id or access")
	}

	err = validateTokenUse()
	if err != nil {
		return err
	}

	// 7. Check the exp claim and make sure the token is not expired.
	err = validateExpired(claims)
	if err != nil {
		return err
	}

	return nil
}

var ErrInvalidClaim = errors.New("invalid claim")

func validateClaimItem(key string, keyShouldBe []string, claims jwtgo.MapClaims) error {
	if val, ok := claims[key]; ok {
		if valStr, ok := val.(string); ok {
			for _, shouldbe := range keyShouldBe {
				// Convert to hash to ensure equal length of each comparable.
				// This is vital for subtle.ConstantTimeCompare() function.
				// Also prevents timing attack guesses against str -> []byte conversion.
				a := blake2b.Sum384([]byte(valStr))
				b := blake2b.Sum384([]byte(shouldbe))
				if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("%w: %q does not match any of valid values: %v", ErrInvalidClaim, key, keyShouldBe)
}

var (
	ErrExpiredToken = errors.New("expired token")
	ErrParseToken   = errors.New("cannot parse token exp")
)

func validateExpired(claims jwtgo.MapClaims) error {
	if tokenExp, ok := claims["exp"]; ok {
		if exp, ok := tokenExp.(float64); ok {
			now := int(time.Now().Unix())
			// Convert user input to a natural number since behavior of
			// subtle.ConstantTimeLessOrEq() is undefined with negative numbers
			absExp := int(math.Abs(exp))
			// This function is prone to year 2038 problem but at least
			// it's protecting against timing attacks
			if subtle.ConstantTimeLessOrEq(now, absExp) == 1 {
				return nil
			}
			return ErrExpiredToken
		}
	}
	return ErrParseToken
}

func convertKey(rawE, rawN string) *rsa.PublicKey {
	decodedE, err := base64.RawURLEncoding.DecodeString(rawE)
	if err != nil {
		panic(err)
	}
	if len(decodedE) < 4 {
		ndata := make([]byte, 4)
		copy(ndata[4-len(decodedE):], decodedE)
		decodedE = ndata
	}
	pubKey := &rsa.PublicKey{
		N: &big.Int{},
		E: int(binary.BigEndian.Uint32(decodedE)),
	}
	decodedN, err := base64.RawURLEncoding.DecodeString(rawN)
	if err != nil {
		panic(err)
	}
	pubKey.N.SetBytes(decodedN)
	return pubKey
}

// Download the json web public key for the given user pool id
func getJWK(jwkURL string) (map[string]JWKKey, error) {
	Info.Printf("Downloading the jwk from the given url %s", jwkURL)
	jwk := &JWK{}

	myClient := &http.Client{Timeout: 10 * time.Second}
	r, err := myClient.Get(jwkURL)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(jwk); err != nil {
		return nil, err
	}

	jwkMap := make(map[string]JWKKey, 0)
	for _, jwk := range jwk.Keys {
		jwkMap[jwk.Kid] = jwk
	}
	return jwkMap, nil
}
