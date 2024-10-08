// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package oidcccl

import (
	"crypto/hmac"
	crypto_rand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	"github.com/cockroachdb/cockroach/pkg/server/authserver"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

// keyAndSignedToken is a container for the two cryptographically bound values that we use to
// ensure that OIDC auth requests and callback are secure. This struct holds a `secretKeyCookie` which
// is an HMAC key encoded into an HTTP cookie, and a `signedTokenEncoded` string which is an
// encoded protobuf object containing a random token and the HMAC hash for that token encoded
// using the key in the cookie.
type keyAndSignedToken struct {
	secretKeyCookie    *http.Cookie
	signedTokenEncoded string
}

// newKeyAndSignedToken creates an instance of `keyAndSignedToken` by randomly generating a key
// and a message of the requested sizes and encoding them into the datatypes we need in order to
// proceed with a secure OIDC auth request.
func newKeyAndSignedToken(
	keySize int, tokenSize int, mode serverpb.OIDCState_Mode,
) (*keyAndSignedToken, error) {
	secretKey := make([]byte, keySize)
	if _, err := crypto_rand.Read(secretKey); err != nil {
		return nil, err
	}

	token := make([]byte, tokenSize)
	if _, err := crypto_rand.Read(token); err != nil {
		return nil, err
	}

	mac := hmac.New(sha256.New, secretKey)
	_, err := mac.Write(token)
	if err != nil {
		return nil, err
	}

	signedTokenEncoded, err := encodeOIDCState(serverpb.OIDCState{
		Token:    token,
		TokenMAC: mac.Sum(nil),
		Mode:     mode,
	})
	if err != nil {
		return nil, err
	}

	secretKeyCookie := authserver.CreateOIDCCookie(
		secretCookieName,
		base64.URLEncoding.EncodeToString(secretKey),
	)

	return &keyAndSignedToken{
		secretKeyCookie,
		signedTokenEncoded,
	}, nil
}

// validate checks the validity of the keyAndSignedToken instance by decoded the protobuf from the
// string type, decoding the HMAC key from the cookie, and recomputing the HMAC to sure that it
// matches the `TokenMAC` field in the protobuf. It returns the result of the equality check from
// the HMAC library.
func (kast *keyAndSignedToken) validate() (bool, serverpb.OIDCState_Mode, error) {
	key, err := base64.URLEncoding.DecodeString(kast.secretKeyCookie.Value)
	if err != nil {
		return false, serverpb.OIDCState_MODE_LOG_IN, err
	}
	mac := hmac.New(sha256.New, key)

	signedToken, err := decodeOIDCState(kast.signedTokenEncoded)
	if err != nil {
		return false, serverpb.OIDCState_MODE_LOG_IN, err
	}

	_, err = mac.Write(signedToken.Token)
	if err != nil {
		return false, serverpb.OIDCState_MODE_LOG_IN, err
	}

	return hmac.Equal(signedToken.TokenMAC, mac.Sum(nil)), signedToken.Mode, nil
}

func encodeOIDCState(statePb serverpb.OIDCState) (string, error) {
	stateBytes, err := protoutil.Marshal(&statePb)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(stateBytes), nil
}

func decodeOIDCState(encodedState string) (*serverpb.OIDCState, error) {
	// Cookie value should be a base64 encoded protobuf.
	stateBytes, err := base64.URLEncoding.DecodeString(encodedState)
	if err != nil {
		return nil, errors.Wrap(err, "state could not be decoded")
	}
	var stateValue serverpb.OIDCState
	if err := protoutil.Unmarshal(stateBytes, &stateValue); err != nil {
		return nil, errors.Wrap(err, "state could not be unmarshaled")
	}
	return &stateValue, nil
}
