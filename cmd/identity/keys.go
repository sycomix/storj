// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package main

import (
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/spf13/cobra"

	"storj.io/storj/pkg/cfgstruct"
	"storj.io/storj/pkg/identity"
	"storj.io/storj/pkg/pkcrypto"
	"storj.io/storj/pkg/process"
	"storj.io/storj/pkg/storj"
)

var (
	keyGenerateCmd = &cobra.Command{
		Use:         "batch-generate",
		Short:       "generate lots of keys",
		RunE:        cmdKeyGenerate,
		Annotations: map[string]string{"type": "setup"},
	}

	keyCfg struct {
		MinDifficulty int    `help:"minimum difficulty to output" default:"30"`
		Concurrency   int    `help:"worker concurrency" default:"4"`
		OutputDir     string `help:"output directory to place keys" default:"."`
	}
)

func init() {
	rootCmd.AddCommand(keyGenerateCmd)
	cfgstruct.Bind(keyGenerateCmd.Flags(), &keyCfg)
}

func cmdKeyGenerate(cmd *cobra.Command, args []string) (err error) {
	ctx := process.Ctx(cmd)
	err = os.MkdirAll(keyCfg.OutputDir, 0700)
	if err != nil {
		return err
	}
	counter := new(uint32)
	return identity.GenerateKeys(ctx, uint16(keyCfg.MinDifficulty), keyCfg.Concurrency,
		func(k *ecdsa.PrivateKey, id storj.NodeID) (done bool, err error) {
			data, err := pkcrypto.PrivateKeyToPKCS8(k)
			if err != nil {
				return false, err
			}
			difficulty, err := id.Difficulty()
			if err != nil {
				return false, err
			}
			filename := fmt.Sprintf("gen-%02d-%d.key", difficulty, atomic.AddUint32(counter, 1))
			fmt.Println("writing", filename)
			err = ioutil.WriteFile(filepath.Join(keyCfg.OutputDir, filename), data, 0600)
			return false, err
		})
}
