// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbstate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/tenderly/nitro/go-ethereum/common"

	"github.com/tenderly/nitro/arbos/util"
	"github.com/tenderly/nitro/blsSignatures"
	"github.com/tenderly/nitro/das/dastree"
)

type DataAvailabilityReader interface {
	GetByHash(ctx context.Context, hash common.Hash) ([]byte, error)
	HealthCheck(ctx context.Context) error
	ExpirationPolicy(ctx context.Context) (ExpirationPolicy, error)
}

var ErrHashMismatch = errors.New("Result does not match expected hash")

// Indicates that this data is a certificate for the data availability service,
// which will retrieve the full batch data.
const DASMessageHeaderFlag byte = 0x80

// Indicates that this DAS certificate data employs the new merkelization strategy.
// Ignored when DASMessageHeaderFlag is not set.
const TreeDASMessageHeaderFlag byte = 0x08

// Indicates that this message was authenticated by L1. Currently unused.
const L1AuthenticatedMessageHeaderFlag byte = 0x40

// Indicates that this message is zeroheavy-encoded.
const ZeroheavyMessageHeaderFlag byte = 0x20

// Indicates that the message is brotli-compressed.
const BrotliMessageHeaderByte byte = 0

func IsDASMessageHeaderByte(header byte) bool {
	return (DASMessageHeaderFlag & header) > 0
}

func IsTreeDASMessageHeaderByte(header byte) bool {
	return (TreeDASMessageHeaderFlag & header) > 0
}

func IsZeroheavyEncodedHeaderByte(header byte) bool {
	return (ZeroheavyMessageHeaderFlag & header) > 0
}

func IsBrotliMessageHeaderByte(b uint8) bool {
	return b == BrotliMessageHeaderByte
}

type DataAvailabilityCertificate struct {
	KeysetHash  [32]byte
	DataHash    [32]byte
	Timeout     uint64
	SignersMask uint64
	Sig         blsSignatures.Signature
	Version     uint8
}

func DeserializeDASCertFrom(rd io.Reader) (c *DataAvailabilityCertificate, err error) {
	r := bufio.NewReader(rd)
	c = &DataAvailabilityCertificate{}

	header, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if !IsDASMessageHeaderByte(header) {
		return nil, errors.New("Tried to deserialize a message that doesn't have the DAS header.")
	}

	_, err = io.ReadFull(r, c.KeysetHash[:])
	if err != nil {
		return nil, err
	}

	_, err = io.ReadFull(r, c.DataHash[:])
	if err != nil {
		return nil, err
	}

	var timeoutBuf [8]byte
	_, err = io.ReadFull(r, timeoutBuf[:])
	if err != nil {
		return nil, err
	}
	c.Timeout = binary.BigEndian.Uint64(timeoutBuf[:])

	if IsTreeDASMessageHeaderByte(header) {
		var versionBuf [1]byte
		_, err = io.ReadFull(r, versionBuf[:])
		if err != nil {
			return nil, err
		}
		c.Version = versionBuf[0]
	}

	var signersMaskBuf [8]byte
	_, err = io.ReadFull(r, signersMaskBuf[:])
	if err != nil {
		return nil, err
	}
	c.SignersMask = binary.BigEndian.Uint64(signersMaskBuf[:])

	var blsSignaturesBuf [96]byte
	_, err = io.ReadFull(r, blsSignaturesBuf[:])
	if err != nil {
		return nil, err
	}
	c.Sig, err = blsSignatures.SignatureFromBytes(blsSignaturesBuf[:])
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *DataAvailabilityCertificate) SerializeSignableFields() []byte {
	buf := make([]byte, 0, 32+9)
	buf = append(buf, c.DataHash[:]...)

	var intData [8]byte
	binary.BigEndian.PutUint64(intData[:], c.Timeout)
	buf = append(buf, intData[:]...)

	if c.Version != 0 {
		buf = append(buf, c.Version)
	}

	return buf
}

func (cert *DataAvailabilityCertificate) RecoverKeyset(
	ctx context.Context,
	da DataAvailabilityReader,
) (*DataAvailabilityKeyset, error) {
	keysetBytes, err := da.GetByHash(ctx, cert.KeysetHash)
	if err != nil {
		return nil, err
	}
	if !dastree.ValidHash(cert.KeysetHash, keysetBytes) {
		return nil, errors.New("keyset hash does not match cert")
	}
	return DeserializeKeyset(bytes.NewReader(keysetBytes))
}

func (cert *DataAvailabilityCertificate) VerifyNonPayloadParts(
	ctx context.Context,
	da DataAvailabilityReader,
) error {
	keyset, err := cert.RecoverKeyset(ctx, da)
	if err != nil {
		return err
	}

	return keyset.VerifySignature(cert.SignersMask, cert.SerializeSignableFields(), cert.Sig)
}

type DataAvailabilityKeyset struct {
	AssumedHonest uint64
	PubKeys       []blsSignatures.PublicKey
}

func (keyset *DataAvailabilityKeyset) Serialize(wr io.Writer) error {
	if err := util.Uint64ToWriter(keyset.AssumedHonest, wr); err != nil {
		return err
	}
	if err := util.Uint64ToWriter(uint64(len(keyset.PubKeys)), wr); err != nil {
		return err
	}
	for _, pk := range keyset.PubKeys {
		pkBuf := blsSignatures.PublicKeyToBytes(pk)
		buf := []byte{byte(len(pkBuf) / 256), byte(len(pkBuf) % 256)}
		_, err := wr.Write(append(buf, pkBuf...))
		if err != nil {
			return err
		}
	}
	return nil
}

func (keyset *DataAvailabilityKeyset) Hash() (common.Hash, error) {
	wr := bytes.NewBuffer([]byte{})
	if err := keyset.Serialize(wr); err != nil {
		return common.Hash{}, err
	}
	if wr.Len() > dastree.BinSize {
		return common.Hash{}, errors.New("keyset too large")
	}
	return dastree.Hash(wr.Bytes()), nil
}

func DeserializeKeyset(rd io.Reader) (*DataAvailabilityKeyset, error) {
	assumedHonest, err := util.Uint64FromReader(rd)
	if err != nil {
		return nil, err
	}
	numKeys, err := util.Uint64FromReader(rd)
	if err != nil {
		return nil, err
	}
	if numKeys > 64 {
		return nil, errors.New("too many keys in serialized DataAvailabilityKeyset")
	}
	pubkeys := make([]blsSignatures.PublicKey, numKeys)
	buf2 := []byte{0, 0}
	for i := uint64(0); i < numKeys; i++ {
		if _, err := io.ReadFull(rd, buf2); err != nil {
			return nil, err
		}
		buf := make([]byte, int(buf2[0])*256+int(buf2[1]))
		if _, err := io.ReadFull(rd, buf); err != nil {
			return nil, err
		}
		pubkeys[i], err = blsSignatures.PublicKeyFromBytes(buf, false)
		if err != nil {
			return nil, err
		}
	}
	return &DataAvailabilityKeyset{
		AssumedHonest: assumedHonest,
		PubKeys:       pubkeys,
	}, nil
}

func (keyset *DataAvailabilityKeyset) VerifySignature(signersMask uint64, data []byte, sig blsSignatures.Signature) error {
	pubkeys := []blsSignatures.PublicKey{}
	numNonSigners := uint64(0)
	for i := 0; i < len(keyset.PubKeys); i++ {
		if (1<<i)&signersMask != 0 {
			pubkeys = append(pubkeys, keyset.PubKeys[i])
		} else {
			numNonSigners++
		}
	}
	if numNonSigners >= keyset.AssumedHonest {
		return errors.New("not enough signers")
	}
	aggregatedPubKey := blsSignatures.AggregatePublicKeys(pubkeys)
	success, err := blsSignatures.VerifySignature(sig, data, aggregatedPubKey)

	if err != nil {
		return err
	}
	if !success {
		return errors.New("bad signature")
	}
	return nil
}

type ExpirationPolicy int64

const (
	KeepForever                ExpirationPolicy = iota // Data is kept forever
	DiscardAfterArchiveTimeout                         // Data is kept till Archive timeout (Archive Timeout is defined by archiving node, assumed to be as long as minimum data timeout)
	DiscardAfterDataTimeout                            // Data is kept till aggregator provided timeout (Aggregator provides a timeout for data while making the put call)
	MixedTimeout                                       // Used for cases with mixed type of timeout policy(Mainly used for aggregators which have data availability services with multiply type of timeout policy)
	DiscardImmediately                                 // Data is never stored (Mainly used for empty/wrapper/placeholder classes)
	// Add more type of expiration policy.
)

func (ep ExpirationPolicy) String() (string, error) {
	switch ep {
	case KeepForever:
		return "KeepForever", nil
	case DiscardAfterArchiveTimeout:
		return "DiscardAfterArchiveTimeout", nil
	case DiscardAfterDataTimeout:
		return "DiscardAfterDataTimeout", nil
	case MixedTimeout:
		return "MixedTimeout", nil
	case DiscardImmediately:
		return "DiscardImmediately", nil
	default:
		return "", errors.New("unknown Expiration Policy")
	}
}

func StringToExpirationPolicy(s string) (ExpirationPolicy, error) {
	switch s {
	case "KeepForever":
		return KeepForever, nil
	case "DiscardAfterArchiveTimeout":
		return DiscardAfterArchiveTimeout, nil
	case "DiscardAfterDataTimeout":
		return DiscardAfterDataTimeout, nil
	case "MixedTimeout":
		return MixedTimeout, nil
	case "DiscardImmediately":
		return DiscardImmediately, nil
	default:
		return -1, fmt.Errorf("invalid Expiration Policy: %s", s)
	}
}
