// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package merklekeystore

import (
	"fmt"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklearray"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-deadlock"
)

// currently, deletion uses rounds, some goroutine runs and calls for deleting and storing
type (
	// EphemeralKeys represent the possible keys inside the keystore.
	// Each key in this struct will be used in a specific round.
	EphemeralKeys struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`

		SignatureAlgorithms []crypto.SignatureAlgorithm `codec:"sks,allocbound=-"`
		// indicates the round that matches SignatureAlgorithms[0].
		FirstRound uint64 `codec:"rnd"`
	}

	// CommittablePublicKey is a key tied to a specific round and is committed by the merklekeystore.Signer.
	CommittablePublicKey struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`

		VerifyingKey crypto.VerifyingKey `codec:"pk"`
		Round        uint64              `codec:"rnd"`
	}

	//Proof represent the merkle proof in each signature.
	//msgp:allocbound Proof -
	Proof []crypto.Digest

	// Signature is a byte signature on a crypto.Hashable object,
	// crypto.VerifyingKey and includes a merkle proof for the key.
	Signature struct {
		_struct              struct{} `codec:",omitempty,omitemptyarray"`
		crypto.ByteSignature `codec:"bsig"`

		Proof        `codec:"prf"`
		VerifyingKey crypto.VerifyingKey `codec:"vkey"`
	}

	// Signer is a merkleKeyStore, contain multiple keys which can be used per round.
	Signer struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`
		// these keys are the keys used to sign in a round.
		// should be disposed of once possible.
		EphemeralKeys EphemeralKeys    `codec:"keys"`
		Tree          merklearray.Tree `codec:"tree"`
		mu            deadlock.RWMutex

		// using this field, the signer can get the accurate location of merkle proof in the merklearray.Tree.
		OriginRound uint64 `codec:"o"`
	}

	// Verifier Is a way to verify a Signature produced by merklekeystore.Signer.
	// it also serves as a commit over all keys contained in the merklekeystore.Signer.
	Verifier struct {
		_struct struct{} `codec:",omitempty,omitemptyarray"`

		Root crypto.Digest `codec:"r"`
	}
)

// ToBeHashed implementation means CommittablePublicKey is crypto.Hashable.
func (e *CommittablePublicKey) ToBeHashed() (protocol.HashID, []byte) {
	return protocol.EphemeralPK, protocol.Encode(e)
}

//Length returns the amount of disposable keys
func (d *EphemeralKeys) Length() uint64 {
	return uint64(len(d.SignatureAlgorithms))
}

// GetHash Gets the hash of the VerifyingKey tied to the signatureAlgorithm in pos.
func (d *EphemeralKeys) GetHash(pos uint64) (crypto.Digest, error) {
	ephPK := CommittablePublicKey{
		VerifyingKey: d.SignatureAlgorithms[pos].GetSigner().GetVerifyingKey(),
		Round:        d.FirstRound + pos,
	}
	return crypto.HashObj(&ephPK), nil
}

var errStartBiggerThanEndRound = fmt.Errorf("cannot create merkleKeyStore because end round is smaller then start round")

// New Generates a merklekeystore.Signer
// Note that the signer will have keys for the rounds  [firstValid, lastValid]
func New(firstValid, lastValid uint64, sigAlgoType crypto.AlgorithmType) (*Signer, error) {
	if firstValid > lastValid {
		return nil, errStartBiggerThanEndRound
	}

	keys := make([]crypto.SignatureAlgorithm, lastValid-firstValid+1)
	for i := range keys {
		keys[i] = *crypto.NewSigner(sigAlgoType)
	}
	ephKeys := EphemeralKeys{
		SignatureAlgorithms: keys,
		FirstRound:          firstValid,
	}
	tree, err := merklearray.Build(&ephKeys)
	if err != nil {
		return nil, err
	}

	return &Signer{
		EphemeralKeys: ephKeys,
		Tree:          *tree,
		mu:            deadlock.RWMutex{},
		OriginRound:   firstValid,
	}, nil
}

// GetVerifier can be used to store the commitment and verifier for this signer.
func (m *Signer) GetVerifier() *Verifier {
	return &Verifier{
		Root: m.Tree.Root(),
	}
}

// how do we dilute the keys? in a way that makes sense?
// in `oneTimeSig` they didn't really dilute keys, but only created what they needed for each round.
// here we know we'll need the keys every 1K messages or so.
// why not distribute the key to cover X rounds where there is at most one comp cert in that round?
// i think that's okay.

// Sign outputs a signature + proof for the signing key.
func (m *Signer) Sign(hashable crypto.Hashable, round uint64) (Signature, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	pos, err := m.getKeyPosition(round)
	if err != nil {
		return Signature{}, err
	}
	// need to get round - originPoint
	proof, err := m.Tree.Prove([]uint64{round - m.OriginRound})
	if err != nil {
		return Signature{}, err
	}

	signer := m.EphemeralKeys.SignatureAlgorithms[pos].GetSigner()
	return Signature{
		ByteSignature: signer.Sign(hashable),
		Proof:         proof,
		VerifyingKey:  signer.GetVerifyingKey(),
	}, nil
}

var errReceivedRoundIsBeforeFirst = fmt.Errorf("round translated to be prior to first key position")
var errOutOfBounds = fmt.Errorf("round translated to be after last key position")

func (m *Signer) getKeyPosition(round uint64) (uint64, error) {
	if round < m.EphemeralKeys.FirstRound {
		return 0, errReceivedRoundIsBeforeFirst
	}

	pos := round - m.EphemeralKeys.FirstRound
	if pos >= uint64(len(m.EphemeralKeys.SignatureAlgorithms)) {
		return 0, errOutOfBounds
	}
	return pos, nil
}

// Trim takes a round, shortness it and outputs the original signer - which can be used for storage.
func (m *Signer) Trim(before uint64) *Signer {
	m.mu.Lock()
	defer m.mu.Unlock()

	pos, err := m.getKeyPosition(before)
	switch err {
	case errReceivedRoundIsBeforeFirst:
		cpy := m.copy()
		return cpy
	case errOutOfBounds:
		m.dropKeys(len(m.EphemeralKeys.SignatureAlgorithms))
	default:
		if pos == 0 {
			return m.copy()
		}
		m.dropKeys(int(pos))
	}
	m.EphemeralKeys.FirstRound = uint64(before)
	cpy := m.copy()

	// Swapping the keys (both of them are the same, but the one in cpy doesn't contain a dangling array behind it.
	// e.g: A=A[len(A)-20:] doesn't mean the garbage collector will free parts of memory from the array.
	// assuming that cpy will be used briefly and then dropped - it's better to swap their key slices.
	m.EphemeralKeys.SignatureAlgorithms, cpy.EphemeralKeys.SignatureAlgorithms =
		cpy.EphemeralKeys.SignatureAlgorithms, m.EphemeralKeys.SignatureAlgorithms

	return cpy
}

func (m *Signer) copy() *Signer {
	signerCopy := Signer{
		_struct: struct{}{},
		EphemeralKeys: EphemeralKeys{
			_struct:             struct{}{},
			SignatureAlgorithms: make([]crypto.SignatureAlgorithm, len(m.EphemeralKeys.SignatureAlgorithms)),
			FirstRound:          m.EphemeralKeys.FirstRound,
		},
		Tree: m.Tree,
		mu:   deadlock.RWMutex{},
	}

	copy(signerCopy.EphemeralKeys.SignatureAlgorithms, m.EphemeralKeys.SignatureAlgorithms)
	return &signerCopy
}

func (m *Signer) dropKeys(upTo int) {
	if l := len(m.EphemeralKeys.SignatureAlgorithms); l < upTo {
		upTo = l
	}
	for i := 0; i < upTo; i++ {
		// zero the keys.
		m.EphemeralKeys.SignatureAlgorithms[i] = crypto.SignatureAlgorithm{}
	}
	m.EphemeralKeys.SignatureAlgorithms = m.EphemeralKeys.SignatureAlgorithms[upTo:]
}

// Verify receives a signature over a specific crypto.Hashable object, and makes certain the signature is correct.
func (v *Verifier) Verify(firstValid, round uint64, obj crypto.Hashable, sig Signature) error {
	if round < firstValid {
		return errReceivedRoundIsBeforeFirst
	}
	ephkey := CommittablePublicKey{
		VerifyingKey: sig.VerifyingKey,
		Round:        round,
	}
	isInTree := merklearray.Verify(v.Root, map[uint64]crypto.Digest{round - firstValid: crypto.HashObj(&ephkey)}, sig.Proof)
	if isInTree != nil {
		return isInTree
	}
	return sig.VerifyingKey.GetVerifier().Verify(obj, sig.ByteSignature)
}
