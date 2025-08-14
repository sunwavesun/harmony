// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package types

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestEIP155Signing(t *testing.T) {
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	signer := NewEIP155Signer(big.NewInt(18))
	tx := NewTx(&LegacyTx{
		Nonce:    0,
		To:       &addr,
		Value:    new(big.Int),
		Gas:      0,
		GasPrice: new(big.Int),
		Data:     nil,
	})
	signedTx, err := SignTx(tx, signer, key)
	if err != nil {
		t.Fatal(err)
	}

	from, err := Sender(signer, signedTx)
	if err != nil {
		t.Fatal(err)
	}
	if from != addr {
		t.Errorf("exected from and address to be equal. Got %x want %x", from, addr)
	}
}

func TestEIP155ChainID(t *testing.T) {
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	signer := NewEIP155Signer(big.NewInt(18))
	tx := NewTx(&LegacyTx{
		Nonce:    0,
		To:       &addr,
		Value:    new(big.Int),
		Gas:      0,
		GasPrice: new(big.Int),
		Data:     nil,
	})
	signedTx, err := SignTx(tx, signer, key)
	if err != nil {
		t.Fatal(err)
	}
	if !signedTx.Protected() {
		t.Fatal("expected tx to be protected")
	}

	if signedTx.ChainId().Cmp(signer.chainID) != 0 {
		t.Error("expected chainID to be", signer.chainID, "got", signedTx.ChainId())
	}

	tx = NewTx(&LegacyTx{
		Nonce:    0,
		To:       &addr,
		Value:    new(big.Int),
		Gas:      0,
		GasPrice: new(big.Int),
		Data:     nil,
	})
	signedTx, err = SignTx(tx, HomesteadSigner{}, key)
	if err != nil {
		t.Fatal(err)
	}

	if signedTx.Protected() {
		t.Error("didn't expect tx to be protected")
	}

	if signedTx.ChainId().Sign() != 0 {
		t.Error("expected chain id to be 0 got", signedTx.ChainId())
	}
}

func TestChainID(t *testing.T) {
	key, _ := defaultTestKey()

	tx := NewTx(&LegacyTx{
		Nonce:    0,
		To:       &common.Address{},
		Value:    new(big.Int),
		Gas:      0,
		GasPrice: new(big.Int),
		Data:     nil,
	})

	var err error
	signedTx, err := SignTx(tx, NewEIP155Signer(big.NewInt(1)), key)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Sender(NewEIP155Signer(big.NewInt(2)), signedTx)
	if err != ErrInvalidChainID {
		t.Error("expected error:", ErrInvalidChainID)
	}

	_, err = Sender(NewEIP155Signer(big.NewInt(1)), signedTx)
	if err != nil {
		t.Error("expected no error")
	}
}
