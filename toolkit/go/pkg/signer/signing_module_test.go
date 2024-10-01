/*
 * Copyright © 2024 Kaleido, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on
 * an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package signer

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	"github.com/hyperledger/firefly-signer/pkg/secp256k1"
	"github.com/kaleido-io/paladin/toolkit/pkg/algorithms"
	"github.com/kaleido-io/paladin/toolkit/pkg/confutil"
	proto "github.com/kaleido-io/paladin/toolkit/pkg/prototk/signer"
	"github.com/kaleido-io/paladin/toolkit/pkg/signer/signerapi"
	"github.com/kaleido-io/paladin/toolkit/pkg/signpayloads"
	"github.com/kaleido-io/paladin/toolkit/pkg/tktypes"
	"github.com/kaleido-io/paladin/toolkit/pkg/verifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testKeyStoreAllFactory struct {
	keyStore *testKeyStoreAll
	err      error
}

type testKeyStoreBaseFactory struct {
	keyStore *testKeyStoreBase
	err      error
}

func (tf *testKeyStoreAllFactory) NewKeyStore(ctx context.Context, conf *signerapi.Config) (signerapi.KeyStore, error) {
	return tf.keyStore, tf.err
}

func (tf *testKeyStoreBaseFactory) NewKeyStore(ctx context.Context, conf *signerapi.Config) (signerapi.KeyStore, error) {
	return tf.keyStore, tf.err
}

type testKeyStoreBase struct {
	findOrCreateLoadableKey func(ctx context.Context, req *proto.ResolveKeyRequest, newKeyMaterial func() ([]byte, error)) (keyMaterial []byte, keyHandle string, err error)
	loadKeyMaterial         func(ctx context.Context, keyHandle string) ([]byte, error)
}

type testKeyStoreAll struct {
	testKeyStoreBase
	findOrCreateInStoreSigningKey func(ctx context.Context, req *proto.ResolveKeyRequest) (res *proto.ResolveKeyResponse, err error)
	signWithinKeystore            func(ctx context.Context, req *proto.SignRequest) (res *proto.SignResponse, err error)
	listKeys                      func(ctx context.Context, req *proto.ListKeysRequest) (res *proto.ListKeysResponse, err error)
}

func (tk *testKeyStoreBase) FindOrCreateLoadableKey(ctx context.Context, req *proto.ResolveKeyRequest, newKeyMaterial func() ([]byte, error)) (keyMaterial []byte, keyHandle string, err error) {
	return tk.findOrCreateLoadableKey(ctx, req, newKeyMaterial)
}

func (tk *testKeyStoreBase) LoadKeyMaterial(ctx context.Context, keyHandle string) ([]byte, error) {
	return tk.loadKeyMaterial(ctx, keyHandle)
}

func (tk *testKeyStoreBase) Close() {}

func (tk *testKeyStoreAll) FindOrCreateInStoreSigningKey(ctx context.Context, req *proto.ResolveKeyRequest) (res *proto.ResolveKeyResponse, err error) {
	return tk.findOrCreateInStoreSigningKey(ctx, req)
}

func (tk *testKeyStoreAll) SignWithinKeystore(ctx context.Context, req *proto.SignRequest) (res *proto.SignResponse, err error) {
	return tk.signWithinKeystore(ctx, req)
}

func (tk *testKeyStoreAll) ListKeys(ctx context.Context, req *proto.ListKeysRequest) (res *proto.ListKeysResponse, err error) {
	return tk.listKeys(ctx, req)
}

type testInMemorySignerFactory struct {
	signer *testMemSigner
	err    error
}

func (tf *testInMemorySignerFactory) NewSigner(ctx context.Context, conf *signerapi.Config) (signerapi.InMemorySigner, error) {
	return tf.signer, tf.err
}

type testMemSigner struct {
	sign             func(ctx context.Context, algorithm, payloadType string, privateKey, payload []byte) ([]byte, error)
	getVerifier      func(ctx context.Context, algorithm, verifierType string, privateKey []byte) (string, error)
	getMinimumKeyLen func(ctx context.Context, algorithm string) (int, error)
}

func (tf *testMemSigner) Sign(ctx context.Context, algorithm, payloadType string, privateKey, payload []byte) ([]byte, error) {
	return tf.sign(ctx, algorithm, payloadType, privateKey, payload)
}

func (tf *testMemSigner) GetVerifier(ctx context.Context, algorithm, verifierType string, privateKey []byte) (string, error) {
	return tf.getVerifier(ctx, algorithm, verifierType, privateKey)
}

func (tf *testMemSigner) GetMinimumKeyLen(ctx context.Context, algorithm string) (int, error) {
	return tf.getMinimumKeyLen(ctx, algorithm)
}

func TestExtensionKeystoreInitFail(t *testing.T) {

	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreBaseFactory{err: fmt.Errorf("pop")},
		},
	}

	_, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "ext-store",
		},
	}, te)
	assert.Regexp(t, "pop", err)

}

func TestExtensionInMemSignerInitFail(t *testing.T) {

	te := &signerapi.Extensions[*signerapi.Config]{
		InMemorySignerFactories: map[string]signerapi.InMemorySignerFactory[*signerapi.Config]{
			"ext-signer": &testInMemorySignerFactory{err: fmt.Errorf("pop")},
		},
	}

	_, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "ext-signer",
		},
	}, te)
	assert.Regexp(t, "pop", err)

}

func TestExtensionNotInKeyStoreSigner(t *testing.T) {

	ks := &testKeyStoreBase{}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreBaseFactory{keyStore: ks, err: nil},
		},
	}

	_, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type:            "ext-store",
			KeyStoreSigning: true,
		},
	}, te)
	assert.Regexp(t, "PD020809", err)

}

func TestKeystoreTypeUnknown(t *testing.T) {

	_, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "unknown",
		},
	})
	assert.Regexp(t, "PD020807", err)

}

func TestKeyDerivationTypeUnknown(t *testing.T) {

	ctx := context.Background()
	_, err := NewSigningModule(ctx, &signerapi.Config{
		KeyDerivation: signerapi.KeyDerivationConfig{
			Type: "unknown",
		},
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeStatic,
		},
	})
	assert.Regexp(t, "PD020819", err)

}

func TestExtensionKeyStoreListOK(t *testing.T) {

	testRes := &proto.ListKeysResponse{
		Items: []*proto.ListKeyEntry{
			{
				Name:      "key 23456",
				KeyHandle: "key23456",
				Identifiers: []*proto.PublicKeyIdentifier{
					{Algorithm: algorithms.ECDSA_SECP256K1, Verifier: "0x93e5a15ce57564278575ff7182b5b3746251e781"},
				},
			},
		},
		Next: "key12345",
	}
	tk := &testKeyStoreAll{
		listKeys: func(ctx context.Context, req *proto.ListKeysRequest) (res *proto.ListKeysResponse, err error) {
			assert.Equal(t, int32(10), req.Limit)
			assert.Equal(t, "key12345", req.Continue)
			return testRes, nil
		},
	}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreAllFactory{keyStore: tk},
		},
	}

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "ext-store",
		},
	}, te)
	require.NoError(t, err)

	res, err := sm.List(context.Background(), &proto.ListKeysRequest{
		Limit:    10,
		Continue: "key12345",
	})
	require.NoError(t, err)
	assert.Equal(t, testRes, res)

	sm.(*signingModule[*signerapi.Config]).disableKeyListing = true
	_, err = sm.List(context.Background(), &proto.ListKeysRequest{
		Limit:    10,
		Continue: "key12345",
	})
	assert.Regexp(t, "PD020815", err)

	sm.Close()
}

func TestExtensionKeyStoreListFail(t *testing.T) {

	tk := &testKeyStoreAll{
		listKeys: func(ctx context.Context, req *proto.ListKeysRequest) (res *proto.ListKeysResponse, err error) {
			return nil, fmt.Errorf("pop")
		},
	}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreAllFactory{keyStore: tk},
		},
	}

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "ext-store",
		},
	}, te)
	require.NoError(t, err)

	_, err = sm.List(context.Background(), &proto.ListKeysRequest{
		Limit:    10,
		Continue: "key12345",
	})
	assert.Regexp(t, "pop", err)

}

func TestExtensionKeyStoreResolveSignSECP256K1OK(t *testing.T) {

	tk := &testKeyStoreAll{
		findOrCreateInStoreSigningKey: func(ctx context.Context, req *proto.ResolveKeyRequest) (res *proto.ResolveKeyResponse, err error) {
			assert.Equal(t, "key1", req.Name)
			return &proto.ResolveKeyResponse{
				KeyHandle: "key_handle_1",
				Identifiers: []*proto.PublicKeyIdentifier{
					{
						Algorithm:    req.RequiredIdentifiers[0].Algorithm,
						VerifierType: req.RequiredIdentifiers[0].VerifierType,
						Verifier:     "0x98a356e0814382587d42b62bd97871ee59d10b69",
					},
				},
			}, nil
		},
		signWithinKeystore: func(ctx context.Context, req *proto.SignRequest) (res *proto.SignResponse, err error) {
			assert.Equal(t, "key_handle_1", req.KeyHandle)
			assert.Equal(t, "something to sign", (string)(req.Payload))
			sig := (&secp256k1.SignatureData{V: big.NewInt(1), R: big.NewInt(2), S: big.NewInt(3)}).CompactRSV()
			return &proto.SignResponse{
				Payload: sig,
			}, nil
		},
	}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreAllFactory{keyStore: tk},
		},
	}

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type:            "ext-store",
			KeyStoreSigning: true,
		},
	}, te)
	require.NoError(t, err)

	resResolve, err := sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: algorithms.ECDSA_SECP256K1, VerifierType: verifiers.ETH_ADDRESS}},
		Name:                "key1",
	})
	require.NoError(t, err)
	assert.Equal(t, "0x98a356e0814382587d42b62bd97871ee59d10b69", resResolve.Identifiers[0].Verifier)

	resSign, err := sm.Sign(context.Background(), &proto.SignRequest{
		KeyHandle:   "key_handle_1",
		Algorithm:   algorithms.Prefix_ECDSA,
		PayloadType: signpayloads.OPAQUE_TO_RSV,
		Payload:     ([]byte)("something to sign"),
	})
	require.NoError(t, err)
	// R, S, V compact encoding
	assert.Equal(t, "0000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000301", hex.EncodeToString(resSign.Payload))
}

func TestExtensionKeyStoreResolveSECP256K1Fail(t *testing.T) {

	tk := &testKeyStoreBase{
		findOrCreateLoadableKey: func(ctx context.Context, req *proto.ResolveKeyRequest, newKeyMaterial func() ([]byte, error)) (keyMaterial []byte, keyHandle string, err error) {
			return nil, "", fmt.Errorf("pop")
		},
	}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreBaseFactory{keyStore: tk},
		},
	}

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: "ext-store",
		},
	}, te)
	require.NoError(t, err)

	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		Name:                "key1",
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: algorithms.ECDSA_SECP256K1, VerifierType: verifiers.ETH_ADDRESS}},
	})
	assert.Regexp(t, "pop", err)

	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{})
	assert.Regexp(t, "PD020820", err)

}

func TestExtensionKeyStoreSignSECP256K1Fail(t *testing.T) {

	tk := &testKeyStoreAll{
		signWithinKeystore: func(ctx context.Context, req *proto.SignRequest) (res *proto.SignResponse, err error) {
			return nil, fmt.Errorf("pop")
		},
	}
	te := &signerapi.Extensions[*signerapi.Config]{
		KeyStoreFactories: map[string]signerapi.KeyStoreFactory[*signerapi.Config]{
			"ext-store": &testKeyStoreAllFactory{keyStore: tk},
		},
	}

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type:            "ext-store",
			KeyStoreSigning: true,
		},
	}, te)
	require.NoError(t, err)

	_, err = sm.Sign(context.Background(), &proto.SignRequest{
		KeyHandle:   "key1",
		Algorithm:   algorithms.ECDSA_SECP256K1,
		PayloadType: signpayloads.OPAQUE_TO_RSV,
		Payload:     ([]byte)("something to sign"),
	})
	assert.Regexp(t, "pop", err)

}

func TestSignInMemoryFailBadKey(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeStatic,
		},
	})
	require.NoError(t, err)

	_, err = sm.Sign(context.Background(), &proto.SignRequest{
		KeyHandle:   "key1",
		Algorithm:   algorithms.ECDSA_SECP256K1,
		PayloadType: signpayloads.OPAQUE_TO_RSV,
		Payload:     ([]byte)("something to sign"),
	})
	assert.Regexp(t, "PD020818", err)

}

func TestResolveSignWithNewKeyCreation(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeFilesystem,
			FileSystem: signerapi.FileSystemConfig{
				Path: confutil.P(t.TempDir()),
			},
		},
	})
	require.NoError(t, err)

	resolveRes, err := sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: algorithms.ECDSA_SECP256K1, VerifierType: verifiers.ETH_ADDRESS}},
		Name:                "key1",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resolveRes.KeyHandle)
	assert.Equal(t, "key1", resolveRes.KeyHandle)
	assert.Equal(t, algorithms.ECDSA_SECP256K1, resolveRes.Identifiers[0].Algorithm)
	assert.NotEmpty(t, resolveRes.Identifiers[0].Verifier)

	signRes, err := sm.Sign(context.Background(), &proto.SignRequest{
		KeyHandle:   resolveRes.KeyHandle,
		Algorithm:   algorithms.ECDSA_SECP256K1,
		PayloadType: signpayloads.OPAQUE_TO_RSV,
		Payload:     ([]byte)("sign me"),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, signRes.Payload)

}

func TestResolveUnsupportedAlgo(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeFilesystem,
			FileSystem: signerapi.FileSystemConfig{
				Path: confutil.P(t.TempDir()),
			},
		},
	})
	require.NoError(t, err)

	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: "wrong"}},
		Name:                "key1",
	})
	assert.Regexp(t, "PD020810.*wrong", err)

}

func TestResolveMissingAlgo(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeFilesystem,
			FileSystem: signerapi.FileSystemConfig{
				Path: confutil.P(t.TempDir()),
			},
		},
	})
	require.NoError(t, err)

	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		Name: "key1",
	})
	assert.Regexp(t, "PD020811", err)

}

func TestResolveLateBindMemSignerError(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyDerivation: signerapi.KeyDerivationConfig{
			Type: signerapi.KeyDerivationTypeBIP32,
		},
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeStatic,
			Static: signerapi.StaticKeyStorageConfig{
				Keys: map[string]signerapi.StaticKeyEntryConfig{
					"seed": {
						Encoding: "hex",
						Inline:   tktypes.RandHex(32),
					},
				},
			},
		},
	})
	require.NoError(t, err)

	testSigner := &testMemSigner{
		getVerifier: func(ctx context.Context, algorithm, verifierType string, privateKey []byte) (string, error) {
			return "", fmt.Errorf("pop")
		},
	}
	sm.AddInMemorySigner("test1", testSigner)
	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		Name:                "test1",
		Index:               0,
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: "test1:any"}},
	})
	assert.Regexp(t, err, "pop")
}

func TestInMemorySignFailures(t *testing.T) {

	sm, err := NewSigningModule(context.Background(), &signerapi.Config{
		KeyStore: signerapi.KeyStoreConfig{
			Type: signerapi.KeyStoreTypeStatic,
			Static: signerapi.StaticKeyStorageConfig{
				Keys: map[string]signerapi.StaticKeyEntryConfig{
					"key1": {
						Encoding: "hex",
						Inline:   "0x00",
					},
				},
			},
		},
	})
	require.NoError(t, err)

	resolveRes, err := sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: algorithms.ECDSA_SECP256K1, VerifierType: verifiers.ETH_ADDRESS}},
		Name:                "key1",
	})
	require.NoError(t, err)

	_, err = sm.Sign(context.Background(), &proto.SignRequest{
		KeyHandle: resolveRes.KeyHandle,
		Payload:   ([]byte)("something to sign"),
	})
	assert.Regexp(t, "PD020810", err)

	_, err = sm.Resolve(context.Background(), &proto.ResolveKeyRequest{
		RequiredIdentifiers: []*proto.PublicKeyIdentifierType{{Algorithm: "wrong"}},
		Name:                "key1",
	})
	assert.Regexp(t, "PD020810", err)

}
