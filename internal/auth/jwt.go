package auth

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type claims struct {
	UserID int64 `json:"uid"`
	RV     int64 `json:"rv"`
	jwt.RegisteredClaims
}

const tokenTTL = 24 * time.Hour

func SignToken(secret []byte, userID, rv int64) (string, error) {
	cl := claims{
		UserID: userID, RV: rv,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(secret)
}

func ParseToken(secret []byte, tokenStr string) (int64, int64, error) {
	var cl claims
	_, err := jwt.ParseWithClaims(tokenStr, &cl, func(t *jwt.Token) (any, error) {
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return 0, 0, err
	}
	return cl.UserID, cl.RV, nil
}
