// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package identity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"sync"
	"sync/atomic"

	"github.com/zeebo/errs"

	"storj.io/storj/pkg/peertls"
	"storj.io/storj/pkg/pkcrypto"
	"storj.io/storj/pkg/storj"
	"storj.io/storj/pkg/utils"
)

const minimumLoggableDifficulty = 8

// PeerCertificateAuthority represents the CA which is used to validate peer identities
type PeerCertificateAuthority struct {
	RestChain []*x509.Certificate
	// Cert is the x509 certificate of the CA
	Cert *x509.Certificate
	// The ID is calculated from the CA public key.
	ID storj.NodeID
}

// FullCertificateAuthority represents the CA which is used to author and validate full identities
type FullCertificateAuthority struct {
	RestChain []*x509.Certificate
	// Cert is the x509 certificate of the CA
	Cert *x509.Certificate
	// The ID is calculated from the CA public key.
	ID storj.NodeID
	// Key is the private key of the CA
	Key crypto.PrivateKey
}

// CASetupConfig is for creating a CA
type CASetupConfig struct {
	ParentCertPath string `help:"path to the parent authority's certificate chain"`
	ParentKeyPath  string `help:"path to the parent authority's private key"`
	CertPath       string `help:"path to the certificate chain for this identity" default:"$IDENTITYDIR/ca.cert"`
	KeyPath        string `help:"path to the private key for this identity" default:"$IDENTITYDIR/ca.key"`
	Difficulty     uint64 `help:"minimum difficulty for identity generation" default:"30"`
	Timeout        string `help:"timeout for CA generation; golang duration string (0 no timeout)" default:"5m"`
	Overwrite      bool   `help:"if true, existing CA certs AND keys will overwritten" default:"false"`
	Concurrency    uint   `help:"number of concurrent workers for certificate authority generation" default:"4"`
}

// NewCAOptions is used to pass parameters to `NewCA`
type NewCAOptions struct {
	// Difficulty is the number of trailing zero-bits the nodeID must have
	Difficulty uint16
	// Concurrency is the number of go routines used to generate a CA of sufficient difficulty
	Concurrency uint
	// ParentCert, if provided will be prepended to the certificate chain
	ParentCert *x509.Certificate
	// ParentKey ()
	ParentKey crypto.PrivateKey
	// Logger is used to log generation status updates
	Logger io.Writer
}

// PeerCAConfig is for locating a CA certificate without a private key
type PeerCAConfig struct {
	CertPath string `help:"path to the certificate chain for this identity" default:"$IDENTITYDIR/ca.cert"`
}

// FullCAConfig is for locating a CA certificate and it's private key
type FullCAConfig struct {
	CertPath string `help:"path to the certificate chain for this identity" default:"$IDENTITYDIR/ca.cert"`
	KeyPath  string `help:"path to the private key for this identity" default:"$IDENTITYDIR/ca.key"`
}

// NewCA creates a new full identity with the given difficulty
func NewCA(ctx context.Context, opts NewCAOptions) (_ *FullCertificateAuthority, err error) {
	defer mon.Task()(&ctx)(&err)
	var (
		highscore = new(uint32)
		i         = new(uint32)

		mu          sync.Mutex
		selectedKey crypto.PrivateKey
		selectedID  storj.NodeID
	)

	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}

	if opts.Logger != nil {
		fmt.Fprintf(opts.Logger, "Generating key with a minimum a difficulty of %d...\n", opts.Difficulty)
	}
	updateStatus := func() {
		if opts.Logger != nil {
			count := atomic.LoadUint32(i)
			hs := atomic.LoadUint32(highscore)
			_, err := fmt.Fprintf(opts.Logger, "\rGenerated %d keys; best difficulty so far: %d", count, hs)
			if err != nil {
				log.Print(errs.Wrap(err))
			}
		}
	}
	err = GenerateKeys(ctx, minimumLoggableDifficulty, int(opts.Concurrency),
		func(k crypto.PrivateKey, id storj.NodeID) (done bool, err error) {
			if opts.Logger != nil {
				if atomic.AddUint32(i, 1)%100 == 0 {
					updateStatus()
				}
			}

			difficulty, err := id.Difficulty()
			if err != nil {
				return false, err
			}
			if difficulty >= opts.Difficulty {
				mu.Lock()
				if selectedKey == nil {
					updateStatus()
					selectedKey = k
					selectedID = id
				}
				mu.Unlock()
				if opts.Logger != nil {
					atomic.SwapUint32(highscore, uint32(difficulty))
					updateStatus()
					_, err := fmt.Fprintf(opts.Logger, "\nFound a key with difficulty %d!\n", difficulty)
					if err != nil {
						log.Print(errs.Wrap(err))
					}
				}
				return true, nil
			}
			for {
				hs := atomic.LoadUint32(highscore)
				if uint32(difficulty) <= hs {
					return false, nil
				}
				if atomic.CompareAndSwapUint32(highscore, hs, uint32(difficulty)) {
					updateStatus()
					return false, nil
				}
			}
		})
	if err != nil {
		return nil, err
	}

	ct, err := peertls.CATemplate()
	if err != nil {
		return nil, err
	}
	c, err := peertls.NewCert(selectedKey, opts.ParentKey, ct, opts.ParentCert)
	if err != nil {
		return nil, err
	}
	ca := &FullCertificateAuthority{
		Cert: c,
		Key:  selectedKey,
		ID:   selectedID,
	}
	if opts.ParentCert != nil {
		ca.RestChain = []*x509.Certificate{opts.ParentCert}
	}
	return ca, nil
}

// Status returns the status of the CA cert/key files for the config
func (caS CASetupConfig) Status() TLSFilesStatus {
	return statTLSFiles(caS.CertPath, caS.KeyPath)
}

// Create generates and saves a CA using the config
func (caS CASetupConfig) Create(ctx context.Context, logger io.Writer) (*FullCertificateAuthority, error) {
	var (
		err    error
		parent *FullCertificateAuthority
	)
	if caS.ParentCertPath != "" && caS.ParentKeyPath != "" {
		parent, err = FullCAConfig{
			CertPath: caS.ParentCertPath,
			KeyPath:  caS.ParentKeyPath,
		}.Load()
		if err != nil {
			return nil, err
		}
	}

	if parent == nil {
		parent = &FullCertificateAuthority{}
	}

	ca, err := NewCA(ctx, NewCAOptions{
		Difficulty:  uint16(caS.Difficulty),
		Concurrency: caS.Concurrency,
		ParentCert:  parent.Cert,
		ParentKey:   parent.Key,
		Logger:      logger,
	})
	if err != nil {
		return nil, err
	}
	caC := FullCAConfig{
		CertPath: caS.CertPath,
		KeyPath:  caS.KeyPath,
	}
	return ca, caC.Save(ca)
}

// FullConfig converts a `CASetupConfig` to `FullCAConfig`
func (caS CASetupConfig) FullConfig() FullCAConfig {
	return FullCAConfig{
		CertPath: caS.CertPath,
		KeyPath:  caS.KeyPath,
	}
}

// Load loads a CA from the given configuration
func (fc FullCAConfig) Load() (*FullCertificateAuthority, error) {
	p, err := fc.PeerConfig().Load()
	if err != nil {
		return nil, err
	}

	kb, err := ioutil.ReadFile(fc.KeyPath)
	if err != nil {
		return nil, peertls.ErrNotExist.Wrap(err)
	}
	k, err := pkcrypto.PrivateKeyFromPEM(kb)
	if err != nil {
		return nil, err
	}

	return &FullCertificateAuthority{
		RestChain: p.RestChain,
		Cert:      p.Cert,
		Key:       k,
		ID:        p.ID,
	}, nil
}

// PeerConfig converts a full ca config to a peer ca config
func (fc FullCAConfig) PeerConfig() PeerCAConfig {
	return PeerCAConfig{
		CertPath: fc.CertPath,
	}
}

// Save saves a CA with the given configuration
func (fc FullCAConfig) Save(ca *FullCertificateAuthority) error {
	var (
		certData, keyData bytes.Buffer
		writeErrs         utils.ErrorGroup
	)

	chain := []*x509.Certificate{ca.Cert}
	chain = append(chain, ca.RestChain...)

	if fc.CertPath != "" {
		if err := peertls.WriteChain(&certData, chain...); err != nil {
			writeErrs.Add(err)
			return writeErrs.Finish()
		}
		if err := writeChainData(fc.CertPath, certData.Bytes()); err != nil {
			writeErrs.Add(err)
			return writeErrs.Finish()
		}
	}

	if fc.KeyPath != "" {
		if err := pkcrypto.WritePrivateKeyPEM(&keyData, ca.Key); err != nil {
			writeErrs.Add(err)
			return writeErrs.Finish()
		}
		if err := writeKeyData(fc.KeyPath, keyData.Bytes()); err != nil {
			writeErrs.Add(err)
			return writeErrs.Finish()
		}
	}

	return writeErrs.Finish()
}

// SaveBackup saves the certificate of the config wth a timestamped filename
func (fc FullCAConfig) SaveBackup(ca *FullCertificateAuthority) error {
	return FullCAConfig{
		CertPath: backupPath(fc.CertPath),
	}.Save(ca)
}

// Load loads a CA from the given configuration
func (pc PeerCAConfig) Load() (*PeerCertificateAuthority, error) {
	chainPEM, err := ioutil.ReadFile(pc.CertPath)
	if err != nil {
		return nil, peertls.ErrNotExist.Wrap(err)
	}

	chain, err := pkcrypto.CertsFromPEM(chainPEM)
	if err != nil {
		return nil, errs.New("failed to load identity %#v: %v",
			pc.CertPath, err)
	}

	nodeID, err := NodeIDFromKey(chain[peertls.LeafIndex].PublicKey)
	if err != nil {
		return nil, err
	}

	return &PeerCertificateAuthority{
		// NB: `CAIndex` is in the context of a complete chain (incl. leaf).
		// Here we're loading the CA chain (nodeID.e. without leaf).
		RestChain: chain[peertls.CAIndex:],
		Cert:      chain[peertls.CAIndex-1],
		ID:        nodeID,
	}, nil
}

// NewIdentity generates a new `FullIdentity` based on the CA. The CA
// cert is included in the identity's cert chain and the identity's leaf cert
// is signed by the CA.
func (ca *FullCertificateAuthority) NewIdentity() (*FullIdentity, error) {
	leafTemplate, err := peertls.LeafTemplate()
	if err != nil {
		return nil, err
	}
	leafKey, err := pkcrypto.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	leafCert, err := peertls.NewCert(leafKey, ca.Key, leafTemplate, ca.Cert)
	if err != nil {
		return nil, err
	}

	if ca.RestChain != nil && len(ca.RestChain) > 0 {
		err := peertls.AddSignedCertExt(ca.Key, leafCert)
		if err != nil {
			return nil, err
		}
	}

	return &FullIdentity{
		RestChain: ca.RestChain,
		CA:        ca.Cert,
		Leaf:      leafCert,
		Key:       leafKey,
		ID:        ca.ID,
	}, nil

}

// RestChainRaw returns the rest (excluding leaf and CA) of the certificate chain as a 2d byte slice
func (ca *FullCertificateAuthority) RestChainRaw() [][]byte {
	var chain [][]byte
	for _, cert := range ca.RestChain {
		chain = append(chain, cert.Raw)
	}
	return chain
}

// Sign signs the passed certificate with ca certificate
func (ca *FullCertificateAuthority) Sign(cert *x509.Certificate) (*x509.Certificate, error) {
	signedCertBytes, err := x509.CreateCertificate(rand.Reader, cert, ca.Cert, cert.PublicKey, ca.Key)
	if err != nil {
		return nil, errs.Wrap(err)
	}

	signedCert, err := pkcrypto.CertFromDER(signedCertBytes)
	if err != nil {
		return nil, errs.Wrap(err)
	}

	return signedCert, nil
}
