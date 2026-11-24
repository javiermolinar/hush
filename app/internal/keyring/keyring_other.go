//go:build !darwin && !linux

package keyring

import "context"

func platformKeyring(_ context.Context, _ request) ([]byte, error) {
	return nil, errUnavailable
}
