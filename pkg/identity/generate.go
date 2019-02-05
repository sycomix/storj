// Copyright (c) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package identity

import (
	"context"
	"crypto/ecdsa"

	"storj.io/storj/pkg/pkcrypto"
	"storj.io/storj/pkg/storj"
)

// GenerateKey generates a private key with a node id with difficulty at least
// minDifficulty. No parallelism is used.
func GenerateKey(ctx context.Context, minDifficulty uint16) (
	k *ecdsa.PrivateKey, id storj.NodeID, err error) {
	var d uint16
	for {
		err = ctx.Err()
		if err != nil {
			break
		}
		k, err = pkcrypto.GeneratePrivateKey()
		if err != nil {
			break
		}
		id, err = NodeIDFromECDSAKey(&k.PublicKey)
		if err != nil {
			break
		}
		d, err = id.Difficulty()
		if err != nil {
			break
		}
		if d >= minDifficulty {
			return k, id, nil
		}
	}
	return k, id, storj.ErrNodeID.Wrap(err)
}

// GenerateCallback indicates that key generation is done when done is true.
// if err != nil key generation will stop with that error
type GenerateCallback func(*ecdsa.PrivateKey, storj.NodeID) (done bool, err error)

// GenerateKeys continues to generate keys until found returns done == false,
// or the ctx is canceled.
func GenerateKeys(ctx context.Context, minDifficulty uint16, concurrency int, found GenerateCallback) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errchan := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				k, id, err := GenerateKey(ctx, minDifficulty)
				if err != nil {
					errchan <- err
					return
				}
				done, err := found(k, id)
				if err != nil {
					errchan <- err
					return
				}
				if done {
					errchan <- nil
					return
				}
			}
		}()
	}

	// we only care about the first error. the rest of the errors will be
	// context cancellation errors
	return <-errchan
}
