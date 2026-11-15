//go:build !windows

package main

import "errors"

func protectSecret(b []byte) ([]byte, error) {
	return nil, errors.New("защищённое сохранение ключей поддерживается только в Windows")
}
func unprotectSecret(b []byte) ([]byte, error) {
	return nil, errors.New("чтение защищённых ключей доступно только в Windows")
}
