package main

import (
	"crypto/subtle"
	"errors"

	"golang.org/x/crypto/ssh"
)

func isAuthorizedKey(authorizedKeys []byte, presented []byte) (bool, error) {
	presentedKey, err := ssh.ParsePublicKey(presented)
	if err != nil {
		return false, err
	}

	if _, ok := presentedKey.(*ssh.Certificate); ok {
		return false, errors.New("ssh certificates are not supported in key mode")
	}

	rest := authorizedKeys
	for len(rest) > 0 {
		parsedKey, _, _, next, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return false, err
		}
		rest = next

		candidate := parsedKey
		if _, ok := parsedKey.(*ssh.Certificate); ok {
			return false, errors.New("authorized_keys must not contain ssh certificates")
		}

		if subtle.ConstantTimeCompare(candidate.Marshal(), presentedKey.Marshal()) == 1 {
			return true, nil
		}
	}

	return false, nil
}
